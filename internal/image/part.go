package image

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// RawConfig содержит настройки для создания raw-образа
type RawConfig struct {
	OutputPath  string
	Size        string // Общий размер образа, например "1G"
	Partitions  []PartitionConfig
	TemplateDir string // Корневая директория шаблона
}

// PartitionConfig содержит настройки для раздела
type PartitionConfig struct {
	Name       string
	Size       string
	Filesystem string
	Mount      string
	Flags      []string
	SourceDir  string // Директория с содержимым для данного раздела
}

// CreateRawImage создает raw-образ с разделами из шаблона
func CreateRawImage(config RawConfig) error {
	// Проверяем наличие необходимых инструментов
	requiredTools := []string{"parted", "mkfs.ext4", "mkfs.vfat", "mount", "umount", "losetup"}
	for _, tool := range requiredTools {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("required tool not found: %s", tool)
		}
	}

	// Создаем выходную директорию, если её нет
	if err := os.MkdirAll(filepath.Dir(config.OutputPath), 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Создаем пустой образ
	fmt.Printf("Creating empty disk image with size %s...\n", config.Size)
	ddCmd := exec.Command("dd", "if=/dev/zero", "of="+config.OutputPath, "bs=1M", "count=0", "seek="+fmt.Sprintf("%d", ParseSizeToMB(config.Size)))
	ddCmd.Stdout = os.Stdout
	ddCmd.Stderr = os.Stderr
	if err := ddCmd.Run(); err != nil {
		return fmt.Errorf("failed to create empty disk image: %w", err)
	}

	// Создаем таблицу разделов
	fmt.Println("Creating partition table...")
	partedCmd := exec.Command("parted", "-s", config.OutputPath, "mklabel", "msdos")
	partedCmd.Stdout = os.Stdout
	partedCmd.Stderr = os.Stderr
	if err := partedCmd.Run(); err != nil {
		return fmt.Errorf("failed to create partition table: %w", err)
	}

	// Полностью заменим метод создания разделов в CreateRawImage
	// Создаем разделы
	fmt.Println("Creating partitions using percentage-based approach...")

	// Используем проценты для определения размеров разделов
	totalFixedSize := 0
	for _, p := range config.Partitions {
		if p.Size != "*" {
			totalFixedSize += ParseSizeToMB(p.Size)
		}
	}

	// Вычисляем общий размер образа в MB
	totalSizeMB := 0
	if strings.HasSuffix(config.Size, "G") {
		sizeGB, _ := strconv.Atoi(strings.TrimSuffix(config.Size, "G"))
		totalSizeMB = sizeGB * 1024
	} else if strings.HasSuffix(config.Size, "M") {
		totalSizeMB, _ = strconv.Atoi(strings.TrimSuffix(config.Size, "M"))
	}

	// Создаем каждый раздел по очереди
	startPercent := 0
	for i, partition := range config.Partitions {
		partitionNumber := i + 1
		fmt.Printf("Creating partition %d: %s\n", partitionNumber, partition.Name)

		var endPercent int
		if partition.Size == "*" {
			// Если размер "*", используем все оставшееся пространство
			endPercent = 100
		} else {
			// Вычисляем процент, который занимает раздел
			partSizeMB := ParseSizeToMB(partition.Size)
			partPercent := (partSizeMB * 100) / totalSizeMB
			endPercent = startPercent + partPercent

			// Убедимся, что не превышаем 100%
			if endPercent > 100 {
				endPercent = 100
			}
		}

		// Создаем раздел с использованием процентов
		fmt.Printf("Partition %s: %d%% to %d%% of disk\n", partition.Name, startPercent, endPercent)

		// Форматируем команду parted для создания раздела
		partedCmd := exec.Command(
			"parted",
			"-s",
			config.OutputPath,
			"mkpart",
			"primary",
			fmt.Sprintf("%d%%", startPercent),
			fmt.Sprintf("%d%%", endPercent),
		)

		partedCmd.Stdout = os.Stdout
		partedCmd.Stderr = os.Stderr
		if err := partedCmd.Run(); err != nil {
			return fmt.Errorf("failed to create partition %s: %w", partition.Name, err)
		}

		// Если раздел загрузочный, устанавливаем флаг boot
		if contains(partition.Flags, "boot") {
			fmt.Printf("Setting boot flag for partition %d\n", partitionNumber)
			partedCmd := exec.Command("parted", "-s", config.OutputPath, "set", strconv.Itoa(partitionNumber), "boot", "on")
			partedCmd.Stdout = os.Stdout
			partedCmd.Stderr = os.Stderr
			if err := partedCmd.Run(); err != nil {
				return fmt.Errorf("failed to set boot flag: %w", err)
			}
		}

		// Обновляем начальный процент для следующего раздела
		startPercent = endPercent
	}

	// Монтируем образ через loop-устройство
	fmt.Println("Setting up loop device...")
	losetupCmd := exec.Command("losetup", "-f", "--show", config.OutputPath)
	losetupOutput, err := losetupCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to setup loop device: %w", err)
	}
	loopDevice := strings.TrimSpace(string(losetupOutput))
	fmt.Printf("Using loop device: %s\n", loopDevice)

	// Освобождаем loop-устройство при выходе
	defer func() {
		fmt.Printf("Detaching loop device %s...\n", loopDevice)
		exec.Command("losetup", "-d", loopDevice).Run()
	}()

	// Форматируем и заполняем разделы
	for i, partition := range config.Partitions {
		// Определяем устройство раздела
		partDevice := fmt.Sprintf("%sp%d", loopDevice, i+1)
		fmt.Printf("Processing partition %s: %s\n", partition.Name, partDevice)

		// Ждем, пока устройство раздела будет доступно
		for i := 0; i < 10; i++ {
			if _, err := os.Stat(partDevice); err == nil {
				break
			}
			fmt.Printf("Waiting for partition device %s to appear...\n", partDevice)
			exec.Command("partprobe", loopDevice).Run()
			time.Sleep(1 * time.Second)
		}

		if _, err := os.Stat(partDevice); os.IsNotExist(err) {
			return fmt.Errorf("partition device %s did not appear", partDevice)
		}

		// Форматируем раздел
		fmt.Printf("Formatting partition %s as %s\n", partition.Name, partition.Filesystem)
		var formatCmd *exec.Cmd
		switch partition.Filesystem {
		case "vfat":
			formatCmd = exec.Command("mkfs.vfat", partDevice)
		case "ext4":
			formatCmd = exec.Command("mkfs.ext4", "-F", partDevice)
		default:
			return fmt.Errorf("unsupported filesystem: %s", partition.Filesystem)
		}

		formatCmd.Stdout = os.Stdout
		formatCmd.Stderr = os.Stderr
		if err := formatCmd.Run(); err != nil {
			return fmt.Errorf("failed to format partition: %w", err)
		}

		// Монтируем раздел и копируем содержимое
		mountPoint := filepath.Join(os.TempDir(), fmt.Sprintf("sysweaver-mount-%s", partition.Name))
		if err := os.MkdirAll(mountPoint, 0755); err != nil {
			return fmt.Errorf("failed to create mount point: %w", err)
		}

		// Монтируем
		fmt.Printf("Mounting %s to %s\n", partDevice, mountPoint)
		mountCmd := exec.Command("mount", partDevice, mountPoint)
		mountCmd.Stdout = os.Stdout
		mountCmd.Stderr = os.Stderr
		if err := mountCmd.Run(); err != nil {
			return fmt.Errorf("failed to mount partition: %w", err)
		}

		// Размонтируем при выходе
		defer func(mountPoint string) {
			fmt.Printf("Unmounting %s...\n", mountPoint)
			exec.Command("umount", mountPoint).Run()
			os.RemoveAll(mountPoint)
		}(mountPoint)

		// Определяем исходную директорию для данного раздела
		var sourceDir string
		if partition.Mount == "/" {
			sourceDir = filepath.Join(config.TemplateDir, "root")
		} else {
			// Берем последний компонент пути монтирования (например, из "/boot" -> "boot")
			dirName := filepath.Base(partition.Mount)
			sourceDir = filepath.Join(config.TemplateDir, dirName)
		}

		// Проверяем существование директории
		if _, err := os.Stat(sourceDir); os.IsNotExist(err) {
			fmt.Printf("Warning: Source directory %s does not exist\n", sourceDir)
			continue
		}

		// Копируем содержимое с учетом типа файловой системы
		fmt.Printf("Copying files from %s to %s\n", sourceDir, mountPoint)

		var cpCmd *exec.Cmd
		if partition.Filesystem == "vfat" {
			// Для VFAT не пытаемся сохранить владельцев файлов
			cpCmd = exec.Command("cp", "-r", "--no-preserve=ownership", sourceDir+"/.", mountPoint+"/")
		} else {
			// Для других файловых систем используем -a (archive)
			cpCmd = exec.Command("cp", "-a", sourceDir+"/.", mountPoint+"/")
		}

		cpCmd.Stdout = os.Stdout
		cpCmd.Stderr = os.Stderr
		if err := cpCmd.Run(); err != nil {
			return fmt.Errorf("failed to copy files to %s: %w", partition.Name, err)
		}
	}

	fmt.Printf("Raw image created successfully: %s\n", config.OutputPath)
	return nil
}

// parseSizeToMB преобразует строку размера (например, "512MiB") в число МБ
func ParseSizeToMB(size string) int {
	size = strings.ToUpper(size)
	multiplier := 1

	if strings.HasSuffix(size, "GIB") || strings.HasSuffix(size, "GB") {
		multiplier = 1024
		size = size[:len(size)-3]
	} else if strings.HasSuffix(size, "G") {
		multiplier = 1024
		size = size[:len(size)-1]
	} else if strings.HasSuffix(size, "MIB") || strings.HasSuffix(size, "MB") {
		size = size[:len(size)-3]
	} else if strings.HasSuffix(size, "M") {
		size = size[:len(size)-1]
	}

	value, _ := strconv.Atoi(size)
	return value * multiplier
}

// contains проверяет, содержится ли элемент в слайсе
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
