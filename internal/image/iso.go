// internal/image/iso.go
package image

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ISOConfig содержит настройки для создания ISO-образа
type ISOConfig struct {
	Label       string // Метка ISO
	Publisher   string // Издатель
	SourcePath  string // Путь к исходным файлам
	OutputPath  string // Путь для сохранения ISO
	Compression string // Метод сжатия (например, "xz")
}

// CreateISO создает ISO-образ из указанной директории
func CreateISO(config ISOConfig) error {
	// Проверяем наличие директории с исходными файлами
	if _, err := os.Stat(config.SourcePath); os.IsNotExist(err) {
		return fmt.Errorf("source directory does not exist: %s", config.SourcePath)
	}

	// Проверяем наличие xorriso
	xorrisoPath, err := exec.LookPath("xorriso")
	if err != nil {
		return fmt.Errorf("xorriso not found in PATH. Please install xorriso")
	}
	fmt.Printf("Using xorriso from: %s\n", xorrisoPath)

	// Создаем директорию для вывода, если она не существует
	outputDir := filepath.Dir(config.OutputPath)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Проверяем, что у нас есть права на запись в выходную директорию
	testFile := filepath.Join(outputDir, ".write_test")
	f, err := os.Create(testFile)
	if err != nil {
		return fmt.Errorf("no write permission to output directory: %w", err)
	}
	f.Close()
	os.Remove(testFile)

	// Проверяем содержимое SourcePath
	fmt.Printf("Checking source directory: %s\n", config.SourcePath)
	files, err := os.ReadDir(config.SourcePath)
	if err != nil {
		return fmt.Errorf("failed to read source directory: %w", err)
	}

	if len(files) == 0 {
		return fmt.Errorf("source directory is empty: %s", config.SourcePath)
	}

	fmt.Printf("Source directory contains %d entries\n", len(files))

	// Команда для создания ISO с xorriso
	args := []string{
		"-as", "mkisofs",
		"-iso-level", "3",
		"-o", config.OutputPath,
		"-volid", config.Label,
		"-publisher", config.Publisher,
		// Исключаем специальные файловые системы
		"-exclude", "proc",
		"-exclude", "sys",
		"-exclude", "dev",
		"-exclude", "tmp",
	}

	// Добавляем загрузочные файлы, если они существуют
	isolinuxPath := filepath.Join(config.SourcePath, "boot/isolinux/isolinux.bin")
	if _, err := os.Stat(isolinuxPath); err == nil {
		fmt.Println("Found isolinux.bin, adding boot options")
		args = append(args,
			"-boot-load-size", "4",
			"-boot-info-table",
			"-b", "boot/isolinux/isolinux.bin",
			"-c", "boot/isolinux/boot.cat",
		)
	} else {
		fmt.Println("Warning: isolinux.bin not found, ISO will not be bootable")
	}

	// Проверка загрузочных файлов
	fmt.Println("\nChecking boot files:")

	// Проверяем структуру ISOLINUX
	isolinuxBin := filepath.Join(config.SourcePath, "boot/isolinux/isolinux.bin")
	filepath.Join(config.SourcePath, "boot/isolinux/boot.cat")
	isolinuxCfg := filepath.Join(config.SourcePath, "boot/isolinux/isolinux.cfg")

	if _, err := os.Stat(isolinuxBin); os.IsNotExist(err) {
		fmt.Println("ERROR: isolinux.bin not found at", isolinuxBin)
		fmt.Println("To create a bootable ISO, add the following to your build scripts:")
		fmt.Println("  mkdir -p /boot/isolinux")
		fmt.Println("  cp /usr/share/syslinux/isolinux.bin /boot/isolinux/")
		fmt.Println("  cp /usr/share/syslinux/ldlinux.c32 /boot/isolinux/")
		fmt.Println("  # Also create a proper isolinux.cfg")
	} else {
		fmt.Println("✅ Found isolinux.bin")

		// Проверяем isolinux.cfg
		if _, err := os.Stat(isolinuxCfg); os.IsNotExist(err) {
			fmt.Println("WARNING: isolinux.cfg not found, boot menu will not be available")
		} else {
			fmt.Println("✅ Found isolinux.cfg")
		}
	}

	// Проверяем EFI
	efiPath := filepath.Join(config.SourcePath, "boot/efi.img")
	if _, err := os.Stat(efiPath); os.IsNotExist(err) {
		fmt.Println("ERROR: efi.img not found at", efiPath)
		fmt.Println("To add EFI boot support, add the following to your build scripts:")
		fmt.Println("  # Install required packages")
		fmt.Println("  apk add grub-efi mtools xorriso")
		fmt.Println("  # Create EFI image")
		fmt.Println("  mkdir -p /boot/efi /boot/grub")
		fmt.Println("  grub-mkimage -o /boot/efi/bootx64.efi -p /boot/grub -O x86_64-efi normal part_gpt fat")
		fmt.Println("  # Create the efi.img file")
		fmt.Println("  dd if=/dev/zero of=/boot/efi.img bs=1M count=4")
		fmt.Println("  mkfs.vfat /boot/efi.img")
		fmt.Println("  # Mount and populate")
		fmt.Println("  mkdir -p /mnt/efi")
		fmt.Println("  mount -o loop /boot/efi.img /mnt/efi")
		fmt.Println("  mkdir -p /mnt/efi/EFI/BOOT")
		fmt.Println("  cp /boot/efi/bootx64.efi /mnt/efi/EFI/BOOT/")
		fmt.Println("  umount /mnt/efi")
	} else {
		fmt.Println("✅ Found efi.img")
	}
	if _, err := os.Stat(efiPath); err == nil {
		// Проверяем размер EFI образа
		efiInfo, err := os.Stat(efiPath)
		if err == nil {
			efiSize := efiInfo.Size() / 1024 // размер в KB
			fmt.Printf("Found EFI image (size: %d KB)\n", efiSize)

			// Если размер не соответствует требованиям El-Torito
			if efiSize != 1200 && efiSize != 1440 && efiSize != 2880 {
				fmt.Println("WARNING: EFI image size does not match required El-Torito sizes (1.2MB, 1.44MB, 2.88MB)")
				fmt.Println("Attempting to resize EFI image to 1.44MB...")

				// Создаем временный образ правильного размера
				tempEfiPath := filepath.Join(os.TempDir(), "efi.img")

				// Создаем пустой образ размером 1.44MB
				ddCmd := exec.Command("dd", "if=/dev/zero", "of="+tempEfiPath, "bs=1k", "count=1440")
				if err := ddCmd.Run(); err != nil {
					fmt.Printf("Warning: could not create temporary EFI image: %v\n", err)
				} else {
					// Копируем содержимое оригинального образа в новый
					ddCmd = exec.Command("dd", "if="+efiPath, "of="+tempEfiPath, "conv=notrunc")
					if err := ddCmd.Run(); err != nil {
						fmt.Printf("Warning: could not copy EFI content: %v\n", err)
					} else {
						// Используем новый образ вместо оригинального
						efiPath = tempEfiPath
						fmt.Println("Successfully resized EFI image to 1.44MB")
					}
				}
			}

			fmt.Println("Adding EFI boot options")
			args = append(args,
				"-eltorito-alt-boot",
				"-e", "boot/efi.img",
				"-no-emul-boot",
				"-isohybrid-gpt-basdat",
			)
		}
	} else {
		fmt.Println("Warning: efi.img not found, ISO will not support EFI boot")
	}

	// Добавляем путь к исходным файлам
	args = append(args, config.SourcePath)

	// Выводим полную команду для диагностики
	fmt.Printf("Running: xorriso %s\n", strings.Join(args, " "))

	// Запускаем команду xorriso
	cmd := exec.Command("xorriso", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create ISO: %w", err)
	}

	// Если требуется сжатие
	if config.Compression != "" {
		switch config.Compression {
		case "xz":
			fmt.Printf("Compressing ISO with xz...\n")
			compressCmd := exec.Command("xz", "-9", config.OutputPath)
			compressCmd.Stdout = os.Stdout
			compressCmd.Stderr = os.Stderr

			if err := compressCmd.Run(); err != nil {
				return fmt.Errorf("failed to compress ISO: %w", err)
			}

			// Переименовываем сжатый файл обратно
			if err := os.Rename(config.OutputPath+".xz", config.OutputPath); err != nil {
				return fmt.Errorf("failed to rename compressed file: %w", err)
			}
		default:
			return fmt.Errorf("unsupported compression method: %s", config.Compression)
		}
	}

	fmt.Printf("ISO image created successfully: %s\n", config.OutputPath)
	return nil
}
