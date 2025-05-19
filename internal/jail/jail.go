package jail

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"sysweaver/internal/config"
	"sysweaver/internal/structures"
)

type Jail struct {
	cmd        *exec.Cmd
	config     structures.JailConfig
	configPath string
	running    bool
	mutex      sync.Mutex
	logWriter  io.Writer
	mounts     []string // Для отслеживания смонтированных ФС

	// Внутренние настройки изоляции
	pidNamespace bool
	uidMappings  []structures.IDMapping
	gidMappings  []structures.IDMapping
}

func NewJail(configPath string, templatePath string) (*Jail, error) {
	var jailConfig structures.JailConfig

	err := config.LoadConfig(configPath, &jailConfig)
	if err != nil {
		return nil, err
	}

	// Проверка обязательных полей
	if jailConfig.ChrootDir == "" {
		return nil, fmt.Errorf("chroot directory not specified in config")
	}

	if jailConfig.BuilderPath == "" {
		return nil, fmt.Errorf("builder path not specified in config")
	}

	// Устанавливаем путь к шаблону из аргумента
	jailConfig.TemplatePath = templatePath

	return &Jail{
		config:       jailConfig,
		configPath:   configPath,
		running:      false,
		logWriter:    os.Stdout,
		mounts:       []string{},
		pidNamespace: true,
		uidMappings: []structures.IDMapping{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		gidMappings: []structures.IDMapping{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
	}, nil
}

// setupMounts настраивает точки монтирования для изолированной среды
func (j *Jail) setupMounts() error {
	// Проверяем существование BuilderPath
	if _, err := os.Stat(j.config.BuilderPath); os.IsNotExist(err) {
		return fmt.Errorf("builder path does not exist: %s", j.config.BuilderPath)
	}

	// Проверяем существование TemplatePath
	if _, err := os.Stat(j.config.TemplatePath); os.IsNotExist(err) {
		return fmt.Errorf("template path does not exist: %s", j.config.TemplatePath)
	}

	// Создаем chroot директорию
	if err := os.MkdirAll(j.config.ChrootDir, 0755); err != nil {
		return fmt.Errorf("failed to create chroot directory: %w", err)
	}

	// Создаем временную директорию для общего монтирования
	// Обе директории (upperdir и workdir) будут находиться в одной точке монтирования
	tmpMountBase := filepath.Join(os.TempDir(), "sysweaver-mount")

	// Очищаем, если существует
	if _, err := os.Stat(tmpMountBase); err == nil {
		if err := os.RemoveAll(tmpMountBase); err != nil {
			return fmt.Errorf("failed to clean temporary mount directory: %w", err)
		}
	}

	// Создаем временную базу и поддиректории
	upperDir := filepath.Join(tmpMountBase, "upper")
	workDir := filepath.Join(tmpMountBase, "work")

	if err := os.MkdirAll(upperDir, 0755); err != nil {
		return fmt.Errorf("failed to create upper directory: %w", err)
	}

	if err := os.MkdirAll(workDir, 0755); err != nil {
		return fmt.Errorf("failed to create work directory: %w", err)
	}

	// Копируем содержимое шаблона в upperDir
	// Используем rsync или find+cp для копирования с сохранением прав
	cmd := exec.Command("sh", "-c", fmt.Sprintf("cp -a %s/* %s/ || true", j.config.TemplatePath, upperDir))
	cmd.Stdout = j.logWriter
	cmd.Stderr = j.logWriter
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(j.logWriter, "Warning: error copying template contents: %v\n", err)
		// Продолжаем, даже если копирование не удалось полностью
	}

	// Монтируем overlay с обновленными параметрами
	overlayOptions := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		j.config.BuilderPath,
		upperDir,
		workDir,
	)

	fmt.Fprintf(j.logWriter, "Mounting overlay with options: %s\n", overlayOptions)

	// Используем mount команду
	mountCmd := exec.Command("mount", "-t", "overlay", "overlay",
		"-o", overlayOptions, j.config.ChrootDir)

	mountCmd.Stdout = j.logWriter
	mountCmd.Stderr = j.logWriter

	if err := mountCmd.Run(); err != nil {
		return fmt.Errorf("failed to mount overlay: %w", err)
	}

	j.mounts = append(j.mounts, j.config.ChrootDir)

	// Монтируем специальные файловые системы
	specialMounts := []struct {
		source  string
		target  string
		fstype  string
		options string
	}{
		{"/proc", filepath.Join(j.config.ChrootDir, "proc"), "proc", ""},
		{"/sys", filepath.Join(j.config.ChrootDir, "sys"), "sysfs", ""},
		{"/dev", filepath.Join(j.config.ChrootDir, "dev"), "devtmpfs", ""},
		{"/dev/pts", filepath.Join(j.config.ChrootDir, "dev/pts"), "devpts", ""},
	}

	for _, m := range specialMounts {
		targetDir := m.target
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			return fmt.Errorf("failed to create mount target %s: %w", targetDir, err)
		}

		mountCmd := exec.Command("mount", "-t", m.fstype, m.source, targetDir)
		if m.options != "" {
			mountCmd.Args = append(mountCmd.Args, "-o", m.options)
		}

		mountCmd.Stdout = j.logWriter
		mountCmd.Stderr = j.logWriter

		if err := mountCmd.Run(); err != nil {
			return fmt.Errorf("failed to mount %s to %s: %w", m.source, targetDir, err)
		}

		j.mounts = append(j.mounts, targetDir)
	}

	// Дополнительные точки монтирования из конфигурации
	for _, mountPoint := range j.config.MountPoints {
		targetDir := filepath.Join(j.config.ChrootDir, mountPoint.Destination)

		// Создаем директорию, если это не файл
		if !strings.Contains(targetDir, ".") {
			if err := os.MkdirAll(targetDir, 0755); err != nil {
				return fmt.Errorf("failed to create mount target directory %s: %w", targetDir, err)
			}
		} else {
			// Если это путь к файлу, создаем родительскую директорию
			parentDir := filepath.Dir(targetDir)
			if err := os.MkdirAll(parentDir, 0755); err != nil {
				return fmt.Errorf("failed to create parent directory for mount target %s: %w", parentDir, err)
			}

			// Создаем пустой файл, если он не существует
			if _, err := os.Stat(targetDir); os.IsNotExist(err) {
				if file, err := os.Create(targetDir); err != nil {
					return fmt.Errorf("failed to create mount target file %s: %w", targetDir, err)
				} else {
					file.Close()
				}
			}
		}

		var mountCmd *exec.Cmd

		// Обрабатываем bind-монтирование отдельно
		if mountPoint.Type == "bind" {
			mountCmd = exec.Command("mount", "--bind", mountPoint.Source, targetDir)
		} else {
			// Для других типов файловых систем
			mountArgs := []string{"-t", mountPoint.Type, mountPoint.Source, targetDir}
			if len(mountPoint.Options) > 0 {
				mountArgs = append(mountArgs, "-o", strings.Join(mountPoint.Options, ","))
			}
			mountCmd = exec.Command("mount", mountArgs...)
		}

		mountCmd.Stdout = j.logWriter
		mountCmd.Stderr = j.logWriter

		if err := mountCmd.Run(); err != nil {
			return fmt.Errorf("failed to mount %s to %s: %w", mountPoint.Source, targetDir, err)
		}

		j.mounts = append(j.mounts, targetDir)
	}

	return nil
}

// Start запускает изолированную среду
func (j *Jail) Start() error {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	if j.running {
		return fmt.Errorf("jail is already running")
	}

	// Настраиваем точки монтирования
	if err := j.setupMounts(); err != nil {
		return err
	}

	// Создаем команду для chroot
	j.cmd = exec.Command("/bin/ash")

	// Настраиваем окружение
	j.cmd.Env = j.config.Environment

	// Настраиваем I/O
	j.cmd.Stdout = j.logWriter
	j.cmd.Stderr = j.logWriter

	// Настраиваем namespaces
	j.cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot: j.config.ChrootDir,
	}

	// Добавляем PID namespace
	if j.pidNamespace {
		j.cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWPID
	}

	// Добавляем User namespace и настраиваем маппинги
	if len(j.uidMappings) > 0 || len(j.gidMappings) > 0 {
		j.cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER

		// Настраиваем UID/GID маппинг
		j.cmd.SysProcAttr.UidMappings = make([]syscall.SysProcIDMap, len(j.uidMappings))
		for i, mapping := range j.uidMappings {
			j.cmd.SysProcAttr.UidMappings[i] = syscall.SysProcIDMap{
				ContainerID: mapping.ContainerID,
				HostID:      mapping.HostID,
				Size:        mapping.Size,
			}
		}

		j.cmd.SysProcAttr.GidMappings = make([]syscall.SysProcIDMap, len(j.gidMappings))
		for i, mapping := range j.gidMappings {
			j.cmd.SysProcAttr.GidMappings[i] = syscall.SysProcIDMap{
				ContainerID: mapping.ContainerID,
				HostID:      mapping.HostID,
				Size:        mapping.Size,
			}
		}
	}

	// Запускаем команду
	if err := j.cmd.Start(); err != nil {
		// Если не удалось запустить, очищаем монтирование
		j.cleanup()
		return fmt.Errorf("failed to start command: %w", err)
	}

	j.running = true
	return nil
}

// cleanup размонтирует все файловые системы
func (j *Jail) cleanup() {
	// Размонтируем в обратном порядке
	for i := len(j.mounts) - 1; i >= 0; i-- {
		umountCmd := exec.Command("umount", j.mounts[i])
		umountCmd.Stdout = j.logWriter
		umountCmd.Stderr = j.logWriter

		if err := umountCmd.Run(); err != nil {
			fmt.Fprintf(j.logWriter, "Warning: failed to unmount %s: %v\n", j.mounts[i], err)
		}
	}
	j.mounts = []string{}
}

// Stop останавливает изолированную среду и освобождает все ресурсы
func (j *Jail) Stop() error {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	if !j.running {
		return fmt.Errorf("jail is not running")
	}

	fmt.Fprintf(j.logWriter, "Cleaning up resources...\n")

	// Останавливаем процесс
	if j.cmd != nil && j.cmd.Process != nil {
		if err := j.cmd.Process.Kill(); err != nil {
			fmt.Fprintf(j.logWriter, "Warning: failed to kill process: %v\n", err)
		}

		// Ждем завершения
		if err := j.cmd.Wait(); err != nil {
			fmt.Fprintf(j.logWriter, "Warning: error waiting for process to exit: %v\n", err)
		}
	}

	// Ищем все точки монтирования внутри chroot директории
	chrootMounts, err := findMountsInPath(j.config.ChrootDir)
	if err != nil {
		fmt.Fprintf(j.logWriter, "Warning: error finding mounts in chroot: %v\n", err)
	}

	// Размонтируем все найденные точки монтирования внутри chroot (в обратном порядке)
	for i := len(chrootMounts) - 1; i >= 0; i-- {
		fmt.Fprintf(j.logWriter, "Unmounting %s...\n", chrootMounts[i])

		// Несколько попыток размонтирования с увеличением агрессивности
		for attempt := 0; attempt < 3; attempt++ {
			var cmd *exec.Cmd

			if attempt == 0 {
				// Первая попытка: обычное размонтирование
				cmd = exec.Command("umount", chrootMounts[i])
			} else if attempt == 1 {
				// Вторая попытка: lazy размонтирование
				cmd = exec.Command("umount", "-l", chrootMounts[i])
			} else {
				// Третья попытка: принудительное размонтирование
				cmd = exec.Command("umount", "-f", chrootMounts[i])
			}

			if err := cmd.Run(); err == nil {
				break // Успешно размонтировано
			} else if attempt == 2 {
				fmt.Fprintf(j.logWriter, "Warning: failed to unmount %s after all attempts\n", chrootMounts[i])
			}

			// Небольшая пауза между попытками
			time.Sleep(100 * time.Millisecond)
		}
	}

	// После размонтирования внутренних точек, размонтируем основные точки
	for i := len(j.mounts) - 1; i >= 0; i-- {
		fmt.Fprintf(j.logWriter, "Unmounting %s...\n", j.mounts[i])

		// Несколько попыток размонтирования
		for attempt := 0; attempt < 3; attempt++ {
			var cmd *exec.Cmd

			if attempt == 0 {
				cmd = exec.Command("umount", j.mounts[i])
			} else if attempt == 1 {
				cmd = exec.Command("umount", "-l", j.mounts[i])
			} else {
				cmd = exec.Command("umount", "-f", j.mounts[i])
			}

			if err := cmd.Run(); err == nil {
				break
			} else if attempt == 2 {
				fmt.Fprintf(j.logWriter, "Warning: failed to unmount %s after all attempts\n", j.mounts[i])
			}

			time.Sleep(100 * time.Millisecond)
		}
	}

	j.mounts = []string{}
	j.running = false

	// Проверка на оставшиеся loop-устройства
	cleanupLoopDevices()

	return nil
}

// findMountsInPath находит все точки монтирования внутри указанного пути
func findMountsInPath(path string) ([]string, error) {
	var mounts []string

	// Чтение /proc/mounts
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil, err
	}

	// Парсинг строк
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			mountPoint := fields[1]

			// Проверяем, является ли точка монтирования дочерней по отношению к path
			if strings.HasPrefix(mountPoint, path) {
				mounts = append(mounts, mountPoint)
			}
		}
	}

	// Сортируем точки монтирования по длине пути (от самых длинных к коротким),
	// чтобы сначала размонтировать вложенные точки
	sort.Slice(mounts, func(i, j int) bool {
		return len(mounts[i]) > len(mounts[j])
	})

	return mounts, nil
}

// cleanupLoopDevices освобождает все loop-устройства, которые могли быть забыты
func cleanupLoopDevices() {
	// Получаем список loop-устройств
	cmd := exec.Command("losetup", "-l", "-n", "-O", "NAME")
	output, err := cmd.Output()
	if err != nil {
		return
	}

	// Отключаем каждое найденное loop-устройство
	devices := strings.Split(string(output), "\n")
	for _, device := range devices {
		device = strings.TrimSpace(device)
		if device != "" {
			exec.Command("losetup", "-d", device).Run()
		}
	}
}

// IsRunning проверяет, выполнена ли изоляция
func (j *Jail) IsRunning() bool {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	return j.running
}

// SetLogWriter устанавливает writer для вывода логов
func (j *Jail) SetLogWriter(writer io.Writer) {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	j.logWriter = writer
}

// ExecuteCommand выполняет команду в изолированной среде с живым выводом
func (j *Jail) ExecuteCommand(command string, args ...string) ([]byte, error) {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	if !j.running {
		return nil, fmt.Errorf("jail is not running")
	}

	// Выводим информацию о выполняемой команде
	fmt.Fprintf(j.logWriter, "Chroot command: %s %s\n", command, strings.Join(args, " "))

	// Запускаем команду в chroot
	cmdArgs := append([]string{j.config.ChrootDir, command}, args...)
	cmd := exec.Command("chroot", cmdArgs...)

	// Создаем pipe для stdout и stderr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Буфер для сбора всего вывода
	var outputBuffer bytes.Buffer
	outputMutex := &sync.Mutex{}

	// Функция для обработки вывода из pipe
	processOutput := func(reader io.Reader, prefix string) {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			line := scanner.Text()

			// Вывести строку в реальном времени
			fmt.Fprintf(j.logWriter, "%s: %s\n", prefix, line)

			// Сохранить строку в буфер
			outputMutex.Lock()
			outputBuffer.WriteString(line)
			outputBuffer.WriteString("\n")
			outputMutex.Unlock()
		}
	}

	// Запускаем горутины для обработки stdout и stderr
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		processOutput(stdoutPipe, "stdout")
	}()

	go func() {
		defer wg.Done()
		processOutput(stderrPipe, "stderr")
	}()

	// Запускаем команду
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start command: %w", err)
	}

	// Ждем завершения команды
	err = cmd.Wait()

	// Ждем завершения обработки вывода
	wg.Wait()

	// Возвращаем собранный вывод и ошибку
	return outputBuffer.Bytes(), err
}

// GetChrootDir возвращает путь к директории chroot
func (j *Jail) GetChrootDir() string {
	j.mutex.Lock()
	defer j.mutex.Unlock()
	return j.config.ChrootDir
}

// IsPidNamespaceEnabled возвращает статус PID namespace
func (j *Jail) IsPidNamespaceEnabled() bool {
	j.mutex.Lock()
	defer j.mutex.Unlock()
	return j.pidNamespace
}

// GetUIDMappings возвращает маппинги UID
func (j *Jail) GetUIDMappings() []structures.IDMapping {
	j.mutex.Lock()
	defer j.mutex.Unlock()
	return j.uidMappings
}

// GetGIDMappings возвращает маппинги GID
func (j *Jail) GetGIDMappings() []structures.IDMapping {
	j.mutex.Lock()
	defer j.mutex.Unlock()
	return j.gidMappings
}

// SetPidNamespace включает или отключает PID namespace
func (j *Jail) SetPidNamespace(enabled bool) {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	if !j.running {
		j.pidNamespace = enabled
	}
}

// SetUIDMappings устанавливает маппинги UID
func (j *Jail) SetUIDMappings(mappings []structures.IDMapping) {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	if !j.running {
		j.uidMappings = mappings
	}
}

// SetGIDMappings устанавливает маппинги GID
func (j *Jail) SetGIDMappings(mappings []structures.IDMapping) {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	if !j.running {
		j.gidMappings = mappings
	}
}
