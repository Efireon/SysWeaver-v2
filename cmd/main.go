package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sysweaver/internal/config"
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
	manual       bool // Новый флаг для ручного режима
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

		// ВАЖНО: Гарантируем очистку ресурсов при выходе, независимо от результата
		cleanup := func() {
			if j != nil && j.IsRunning() {
				fmt.Println("Cleaning up resources...")
				if stopErr := j.Stop(); stopErr != nil {
					fmt.Printf("Warning: error during cleanup: %v\n", stopErr)
				}
			}
		}

		// Используем defer для гарантированного выполнения cleanup
		// Не пропускаем cleanup даже в ручном режиме, чтобы предотвратить утечку ресурсов
		defer cleanup()

		// Включаем verbose режим, если указан
		if verbose {
			j.SetLogWriter(os.Stdout)
		}

		// Создаем директорию output внутри chroot
		outputDirInChroot := filepath.Join(j.GetChrootDir(), "output")
		if err := os.MkdirAll(outputDirInChroot, 0755); err != nil {
			return fmt.Errorf("error creating output directory in chroot: %w", err)
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

			// В зависимости от режима выполняем скрипт
			var err error
			if verbose {
				// В verbose режиме - live вывод
				fmt.Println("--- Live output ---")
				_, err = j.ExecuteCommand("/bin/sh", chrootScriptPath)
			} else {
				// В обычном режиме - собираем вывод и показываем после
				output, execErr := j.ExecuteCommandWithOutput("/bin/sh", chrootScriptPath)
				err = execErr

				// Вычисляем время выполнения
				duration := time.Since(startTime)

				// Выводим результаты выполнения
				if err != nil {
					fmt.Printf("❌ Script failed (%.2f seconds): %v\n", duration.Seconds(), err)
					fmt.Println("--- Output begin ---")
					fmt.Println(string(output))
					fmt.Println("--- Output end ---")

					// Если мы в ручном режиме, позволяем пользователю исследовать состояние
					if manual {
						fmt.Println("\nEntering manual mode for debugging. Type 'exit' to quit.")

						// Запускаем интерактивную оболочку
						shellCmd := exec.Command("sudo", "chroot", j.GetChrootDir(), "/bin/sh")
						shellCmd.Stdin = os.Stdin
						shellCmd.Stdout = os.Stdout
						shellCmd.Stderr = os.Stderr

						if shellErr := shellCmd.Run(); shellErr != nil {
							fmt.Printf("Error in interactive shell: %v\n", shellErr)
						}

						fmt.Println("Exited from manual mode, continuing with cleanup...")
					}

					// Возвращаем ошибку - cleanup будет выполнен через defer
					return fmt.Errorf("error executing script %s: %v", scriptName, err)
				}

				// Если скрипт выполнился успешно, выводим время
				fmt.Printf("✅ Script completed successfully in %.2f seconds\n", duration.Seconds())

				// Показываем краткий вывод или полный в зависимости от размера
				if len(output) < 500 {
					if len(output) > 0 {
						fmt.Println("--- Output begin ---")
						fmt.Println(string(output))
						fmt.Println("--- Output end ---")
					}
				} else {
					// Если вывод длинный, показываем только начало и конец
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
				continue // Переходим к следующему скрипту, так как время уже подсчитано
			}

			// Вычисляем время выполнения (только для verbose режима)
			duration := time.Since(startTime)

			// Выводим результаты выполнения
			if err != nil {
				fmt.Printf("❌ Script failed (%.2f seconds): %v\n", duration.Seconds(), err)

				// Если мы в ручном режиме, позволяем пользователю исследовать состояние
				if manual {
					fmt.Println("\nEntering manual mode for debugging. Type 'exit' to quit.")

					// Запускаем интерактивную оболочку
					shellCmd := exec.Command("sudo", "chroot", j.GetChrootDir(), "/bin/sh")
					shellCmd.Stdin = os.Stdin
					shellCmd.Stdout = os.Stdout
					shellCmd.Stderr = os.Stderr

					if shellErr := shellCmd.Run(); shellErr != nil {
						fmt.Printf("Error in interactive shell: %v\n", shellErr)
					}

					fmt.Println("Exited from manual mode, continuing with cleanup...")
				}

				return fmt.Errorf("error executing script %s: %v", scriptName, err)
			}

			// Если скрипт выполнился успешно, выводим время
			fmt.Printf("✅ Script completed successfully in %.2f seconds\n", duration.Seconds())
		}

		fmt.Println("\n✅ All installation scripts completed successfully!")

		if manual {
			// Если включен ручной режим, даем пользователю возможность войти в jail
			fmt.Println("\nEntering manual mode. Type 'exit' to quit and continue.")

			// Запускаем интерактивную оболочку
			shellCmd := exec.Command("sudo", "chroot", j.GetChrootDir(), "/bin/sh")
			shellCmd.Stdin = os.Stdin
			shellCmd.Stdout = os.Stdout
			shellCmd.Stderr = os.Stderr

			if err := shellCmd.Run(); err != nil {
				fmt.Printf("Error in interactive shell: %v\n", err)
			}

			fmt.Println("Exited from manual mode, continuing with image copying...")
		}

		fmt.Println("\n✅ All installation scripts completed successfully!")

		// Копируем готовые образы из chroot в указанную директорию вывода
		fmt.Println("\nCopying built images from jail...")

		// Создаем директорию для вывода, если она не существует
		if err := os.MkdirAll(outputPath, 0755); err != nil {
			return fmt.Errorf("error creating output directory: %w", err)
		}

		// Ищем файлы в /output внутри chroot
		outputFiles, err := filepath.Glob(filepath.Join(outputDirInChroot, "*"))
		if err != nil {
			return fmt.Errorf("error searching for output files: %w", err)
		}

		if len(outputFiles) == 0 {
			fmt.Println("Warning: No output files found in /output directory inside jail.")
		} else {
			// Копируем каждый файл
			for _, file := range outputFiles {
				fileName := filepath.Base(file)
				destPath := filepath.Join(outputPath, fileName)

				fmt.Printf("Copying %s to %s\n", fileName, destPath)

				// Копируем файл
				input, err := os.Open(file)
				if err != nil {
					return fmt.Errorf("error opening source file: %w", err)
				}
				defer input.Close()

				output, err := os.Create(destPath)
				if err != nil {
					input.Close() // Закрываем входной файл при ошибке
					return fmt.Errorf("error creating destination file: %w", err)
				}
				defer output.Close()

				if _, err := io.Copy(output, input); err != nil {
					return fmt.Errorf("error copying file: %w", err)
				}

				fmt.Printf("Successfully copied %s\n", fileName)
			}
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
	buildCmd.Flags().BoolVarP(&manual, "manual", "m", false, "Enter manual mode after scripts execution")

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
