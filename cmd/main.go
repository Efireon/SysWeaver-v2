package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sysweaver/internal/config"
	"sysweaver/internal/image"
	"sysweaver/internal/jail"
	"sysweaver/internal/structures"
	"time"

	"github.com/spf13/cobra"
)

var (
	// Флаги
	templatePath string
	outputPath   string
	configPath   string
	verbose      bool
)

// rootCmd представляет базовую команду
var rootCmd = &cobra.Command{
	Use:   "sysweaver",
	Short: "SysWeaver - tool for building custom Linux images",
	Long: `SysWeaver is a flexible and efficient tool for creating custom Linux images
with Alpine Linux as the base operating system.
`,
	Run: func(cmd *cobra.Command, args []string) {
		// Если команда запущена без подкоманд, выводим помощь
		cmd.Help()
	},
}

// buildCmd представляет команду для создания образа
var buildCmd = &cobra.Command{
	Use:   "build [template]",
	Short: "Build a Linux image from a template",
	Long: `Build a Linux image using the specified template.
The template should contain all necessary scripts and configurations.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		templatePath, err := filepath.Abs(args[0])
		if err != nil {
			return fmt.Errorf("error resolving template path: %w", err)
		}

		// Если configPath не указан, используем config.yaml из шаблона
		if configPath == "" {
			configPath = filepath.Join(templatePath, "config.yaml")
		}

		fmt.Printf("Building image from template: %s\n", templatePath)
		fmt.Printf("Using config: %s\n", configPath)
		fmt.Printf("Output will be saved to: %s\n", outputPath)

		// Загружаем общую конфигурацию
		var buildConfig structures.BuildConfig
		if err := config.LoadConfig(configPath, &buildConfig); err != nil {
			return fmt.Errorf("error loading build config: %w", err)
		}

		// Загружаем конфигурацию jail из шаблона
		jailConfigPath := filepath.Join(templatePath, "jail.yaml")

		// Создаем Jail
		j, err := jail.NewJail(jailConfigPath, templatePath)
		if err != nil {
			return fmt.Errorf("error creating jail: %w", err)
		}

		// Гарантируем очистку ресурсов при выходе
		defer func() {
			if j.IsRunning() {
				fmt.Println("Cleaning up resources...")
				j.Stop()
			}
		}()

		// Включаем verbose режим, если указан
		if verbose {
			j.SetLogWriter(os.Stdout)
		}

		// Запускаем изолированную среду
		if err := j.Start(); err != nil {
			return fmt.Errorf("error starting jail: %w", err)
		}

		// Собираем скрипты из шаблона
		scriptsDir := filepath.Join(templatePath, "scripts/install")
		scripts, err := getScriptsInOrder(scriptsDir)
		if err != nil {
			return fmt.Errorf("error getting scripts: %w", err)
		}

		// Добавляем информацию о общем числе скриптов
		totalScripts := len(scripts)
		fmt.Printf("Found %d installation scripts\n", totalScripts)

		// Выполняем скрипты
		for i, script := range scripts {
			// Получаем только имя скрипта (без пути)
			scriptName := filepath.Base(script)

			// Добавляем информацию о прогрессе
			fmt.Printf("==============================\n")
			fmt.Printf("Executing script [%d/%d]: %s\n", i+1, totalScripts, scriptName)
			fmt.Printf("==============================\n")

			// Замеряем время выполнения
			startTime := time.Now()

			// Путь к скрипту внутри chroot
			chrootScriptPath := "/scripts/install/" + scriptName

			// Запускаем скрипт внутри chroot с правильным путем
			output, err := j.ExecuteCommand("/bin/sh", chrootScriptPath)

			// Вычисляем время выполнения
			duration := time.Since(startTime)

			// Выводим результаты выполнения
			if err != nil {
				fmt.Printf("❌ Script failed (%.2f seconds): %v\n", duration.Seconds(), err)
				fmt.Println("--- Output begin ---")
				fmt.Println(string(output))
				fmt.Println("--- Output end ---")
				return fmt.Errorf("error executing script %s: %v", scriptName, err)
			}

			// Если скрипт выполнился успешно, выводим время
			fmt.Printf("✅ Script completed successfully in %.2f seconds\n", duration.Seconds())

			// В verbose режиме или если вывод короткий, показываем его в любом случае
			if verbose || len(output) < 500 {
				fmt.Println("--- Output begin ---")
				fmt.Println(string(output))
				fmt.Println("--- Output end ---")
			} else {
				// Если вывод длинный и не verbose, показываем только начало и конец
				lines := strings.Split(string(output), "\n")
				if len(lines) > 10 {
					fmt.Println("--- Output preview (use --verbose for full output) ---")
					for i := 0; i < 5; i++ {
						if i < len(lines) {
							fmt.Println(lines[i])
						}
					}
					fmt.Println("...")
					for i := len(lines) - 5; i < len(lines); i++ {
						if i >= 0 && i < len(lines) {
							fmt.Println(lines[i])
						}
					}
					fmt.Println("--- End of preview ---")
				} else {
					fmt.Println("--- Output begin ---")
					fmt.Println(string(output))
					fmt.Println("--- Output end ---")
				}
			}
		}

		fmt.Println("\n✅ All installation scripts completed successfully!")

		if len(buildConfig.Partitions) > 0 {
			fmt.Println("\n=== Creating raw disk image ===")

			// Преобразуем конфигурацию разделов из buildConfig
			partitions := make([]image.PartitionConfig, len(buildConfig.Partitions))

			// Вычисляем общий размер образа на основе размеров разделов
			totalSize := 0
			hasUnboundedPartition := false

			for i, p := range buildConfig.Partitions {
				partitions[i] = image.PartitionConfig{
					Name:       p.Name,
					Size:       p.Size,
					Filesystem: p.Filesystem,
					Mount:      p.Mount,
					Flags:      p.Flags,
				}

				fmt.Printf("Adding partition from config: %s (%s, %s)\n", p.Name, p.Mount, p.Filesystem)

				// Суммируем размеры всех разделов с фиксированным размером
				if p.Size != "*" {
					partSize := image.ParseSizeToMB(p.Size)
					totalSize += partSize
					fmt.Printf("Partition %s size: %d MB\n", p.Name, partSize)
				} else {
					hasUnboundedPartition = true
					fmt.Printf("Partition %s has dynamic size (*)\n", p.Name)
				}
			}

			// Добавляем дополнительное пространство для раздела с "*" и учитываем служебную информацию
			if hasUnboundedPartition {
				// Добавляем 1GB для раздела с "*" плюс 10% запаса
				totalSize += 1024 // 1GB для раздела с "*"
			}

			// Добавляем 10% запаса и минимум 50MB для служебной информации
			totalSize = totalSize + int(float64(totalSize)*0.1) + 50

			// Конвертируем обратно в строку с единицей измерения
			var imgSize string
			if totalSize >= 1024 {
				// Если больше 1GB, выражаем в GB
				imgSize = fmt.Sprintf("%dG", (totalSize+1023)/1024) // Округляем вверх
			} else {
				imgSize = fmt.Sprintf("%dM", totalSize)
			}

			rawConfig := image.RawConfig{
				OutputPath:  filepath.Join(outputPath, fmt.Sprintf("%s-%s.img", buildConfig.Name, time.Now().Format("20060102-150405"))),
				Size:        imgSize,
				Partitions:  partitions,
				TemplateDir: templatePath,
			}

			fmt.Printf("Creating image with calculated size: %s based on %d partitions\n",
				imgSize, len(partitions))

			if err := image.CreateRawImage(rawConfig); err != nil {
				return fmt.Errorf("error creating raw image: %w", err)
			}
		}

		// Создаем ISO-образ
		isoConfig := image.ISOConfig{
			Label:       buildConfig.ISO.Label,
			Publisher:   buildConfig.ISO.Publisher,
			SourcePath:  j.GetChrootDir(),
			OutputPath:  filepath.Join(outputPath, fmt.Sprintf("%s-%s.iso", buildConfig.Name, time.Now().Format("20060102-150405"))),
			Compression: buildConfig.ISO.Compression,
		}

		if err := image.CreateISO(isoConfig); err != nil {
			return fmt.Errorf("error creating ISO: %w", err)
		}

		fmt.Println("Build completed successfully!")
		return nil
	},
}

// createTemplateCmd представляет команду для создания нового шаблона
var createTemplateCmd = &cobra.Command{
	Use:   "create-template [name]",
	Short: "Create a new template",
	Long: `Create a new template with the basic structure for building images.
The template will include example scripts and configuration files.`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]
		fmt.Printf("Creating new template: %s\n", name)

		// TODO: Реализовать создание шаблона
		// 1. Создать структуру директорий
		// 2. Добавить базовые скрипты
		// 3. Создать config.yaml
	},
}

// validateCmd представляет команду для проверки шаблона
var validateCmd = &cobra.Command{
	Use:   "validate [template]",
	Short: "Validate a template",
	Long: `Validate the structure and configuration of a template.
This command checks if the template has all necessary files and
if the configuration is valid.`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		templatePath := args[0]
		fmt.Printf("Validating template: %s\n", templatePath)

		// TODO: Реализовать валидацию шаблона
		// 1. Проверить структуру директорий
		// 2. Проверить config.yaml
		// 3. Проверить скрипты
	},
}

// versionCmd представляет команду для отображения версии
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number of SysWeaver",
	Long:  `All software has versions. This is SysWeaver's.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("SysWeaver v0.1.0")
	},
}

// getScriptsInOrder возвращает список скриптов из директории в порядке их выполнения
func getScriptsInOrder(dir string) ([]string, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var scripts []string
	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".sh" {
			scripts = append(scripts, filepath.Join(dir, file.Name()))
		}
	}

	// TODO: Сортировка скриптов по имени

	return scripts, nil
}

func init() {
	// Глобальные флаги
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")

	// Флаги для кома`нды build
	buildCmd.Flags().StringVarP(&outputPath, "output", "o", "./output", "Output directory for the built image")
	buildCmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to the configuration file (defaults to template/config.yaml)")

	// Добавляем подкоманды
	rootCmd.AddCommand(buildCmd)
	rootCmd.AddCommand(versionCmd)

	// Отключаем вывод справки при ошибках
	buildCmd.SilenceUsage = true
	buildCmd.SilenceErrors = true

	// Также для rootCmd
	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
