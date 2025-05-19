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
		fmt.Println("Warning: isolinux.bin not found, ISO will not be bootable via BIOS")
	}

	// Добавляем EFI загрузку, если она есть
	efiPath := filepath.Join(config.SourcePath, "boot/efi.img")
	if _, err := os.Stat(efiPath); err == nil {
		efiSize := "Skipping EFI boot for now - will be enabled in future versions"
		fmt.Println(efiSize)

		// НЕ добавляем опции EFI для MVP
		/*
			args = append(args,
				"-eltorito-alt-boot",
				"-e", "boot/efi.img",
				"-no-emul-boot",
				"-isohybrid-gpt-basdat",
			)
		*/
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
