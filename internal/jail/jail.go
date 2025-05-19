package jail

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

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

	// Создаем временную директорию для overlay
	tmpMountBase := filepath.Join(os.TempDir(), "sysweaver-mount")

	// Очищаем, если существует
	if _, err := os.Stat(tmpMountBase); err == nil {
		if err := os.RemoveAll(tmpMountBase); err != nil {
			return fmt.Errorf("failed to clean temporary mount directory: %w", err)
		}
	}

	// Создаем временную базу и поддиректории для overlay
	upperDir := filepath.Join(tmpMountBase, "upper")
	workDir := filepath.Join(tmpMountBase, "work")

	if err := os.MkdirAll(upperDir, 0755); err != nil {
		return fmt.Errorf("failed to create upper directory: %w", err)
	}

	if err := os.MkdirAll(workDir, 0755); err != nil {
		return fmt.Errorf("failed to create work directory: %w", err)
	}

	// Монтируем overlay с билдером как основой
	overlayOptions := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		j.config.BuilderPath,
		upperDir,
		workDir,
	)

	fmt.Fprintf(j.logWriter, "Mounting overlay with options: %s\n", overlayOptions)

	// Используем mount команду для overlay
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

	// Монтируем шаблон в специальные точки внутри chroot
	if err := j.mountTemplate(); err != nil {
		return fmt.Errorf("failed to mount template: %w", err)
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

// mountTemplate монтирует компоненты шаблона в соответствующие точки
func (j *Jail) mountTemplate() error {
	templateMounts := []struct {
		source   string
		target   string
		name     string
		writable bool // Нужна ли запись в эту директорию
	}{
		{j.config.TemplatePath, "/template", "template root", false},
		{filepath.Join(j.config.TemplatePath, "scripts"), "/scripts", "template scripts", false},
	}

	for _, tmpl := range templateMounts {
		// Проверяем существование исходной директории
		if _, err := os.Stat(tmpl.source); os.IsNotExist(err) {
			fmt.Fprintf(j.logWriter, "Template directory %s not found, skipping %s\n", tmpl.source, tmpl.name)
			continue
		}

		targetDir := filepath.Join(j.config.ChrootDir, tmpl.target)

		// Создаем целевую директорию
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			return fmt.Errorf("failed to create template mount directory %s: %w", targetDir, err)
		}

		// Монтируем как bind mount с правильными опциями
		var mountCmd *exec.Cmd
		if tmpl.writable {
			// Для записываемых директорий - обычный bind mount
			mountCmd = exec.Command("mount", "--bind", tmpl.source, targetDir)
		} else {
			// Для читаемых директорий - read-only bind mount
			mountCmd = exec.Command("mount", "--bind", tmpl.source, targetDir)
		}

		mountCmd.Stdout = j.logWriter
		mountCmd.Stderr = j.logWriter

		if err := mountCmd.Run(); err != nil {
			fmt.Fprintf(j.logWriter, "Warning: failed to bind mount %s: %v\n", tmpl.name, err)
			continue
		}

		// Для read-only директорий добавляем remount с ro опцией
		if !tmpl.writable {
			remountCmd := exec.Command("mount", "-o", "remount,ro,bind", targetDir)
			remountCmd.Stdout = j.logWriter
			remountCmd.Stderr = j.logWriter

			if err := remountCmd.Run(); err != nil {
				fmt.Fprintf(j.logWriter, "Warning: failed to remount %s as read-only: %v\n", tmpl.name, err)
			}
		}

		fmt.Fprintf(j.logWriter, "Successfully mounted %s to %s\n", tmpl.name, targetDir)
		j.mounts = append(j.mounts, targetDir)
	}

	// Дополнительно: копируем содержимое шаблона в upperdir для записи
	// Это позволит скриптам изменять скопированные файлы без проблем с правами
	return j.copyTemplateToUpper()
}

// copyTemplateToUpper копирует содержимое шаблона в upper слой overlay
func (j *Jail) copyTemplateToUpper() error {
	// Находим upper директорию в текущих mount'ах
	upperDir := ""

	// Читаем /proc/mounts чтобы найти overlay с нашим chroot
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return fmt.Errorf("failed to read /proc/mounts: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == j.config.ChrootDir && fields[2] == "overlay" {
			// Парсим опции overlay
			options := strings.Split(fields[3], ",")
			for _, opt := range options {
				if strings.HasPrefix(opt, "upperdir=") {
					upperDir = strings.TrimPrefix(opt, "upperdir=")
					break
				}
			}
			break
		}
	}

	if upperDir == "" {
		return fmt.Errorf("could not find upperdir for overlay")
	}

	// Копируем содержимое template/root в upperdir, изменяя владельца на текущего пользователя
	templateRoot := filepath.Join(j.config.TemplatePath, "root")
	if _, err := os.Stat(templateRoot); err == nil {
		fmt.Fprintf(j.logWriter, "Copying template root to upper layer...\n")

		// Используем rsync для копирования с правильными правами
		rsyncCmd := exec.Command("rsync", "-av", "--chown="+fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), templateRoot+"/", upperDir+"/")
		rsyncCmd.Stdout = j.logWriter
		rsyncCmd.Stderr = j.logWriter

		if err := rsyncCmd.Run(); err != nil {
			// Если rsync не удался, пробуем обычное копирование
			fmt.Fprintf(j.logWriter, "Rsync failed, trying cp...\n")
			cpCmd := exec.Command("cp", "-a", templateRoot+"/.", upperDir+"/")
			cpCmd.Stdout = j.logWriter
			cpCmd.Stderr = j.logWriter

			if err := cpCmd.Run(); err != nil {
				return fmt.Errorf("failed to copy template root: %w", err)
			}
		}
	}

	// Копируем содержимое template/boot, если оно есть
	templateBoot := filepath.Join(j.config.TemplatePath, "boot")
	if _, err := os.Stat(templateBoot); err == nil {
		fmt.Fprintf(j.logWriter, "Copying template boot to upper layer...\n")

		upperBoot := filepath.Join(upperDir, "boot")
		if err := os.MkdirAll(upperBoot, 0755); err != nil {
			return fmt.Errorf("failed to create boot directory in upper: %w", err)
		}

		// Используем rsync для boot файлов
		rsyncCmd := exec.Command("rsync", "-av", "--chown="+fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), templateBoot+"/", upperBoot+"/")
		rsyncCmd.Stdout = j.logWriter
		rsyncCmd.Stderr = j.logWriter

		if err := rsyncCmd.Run(); err != nil {
			// Если rsync не удался, пробуем обычное копирование
			fmt.Fprintf(j.logWriter, "Rsync failed for boot, trying cp...\n")
			cpCmd := exec.Command("cp", "-a", templateBoot+"/.", upperBoot+"/")
			cpCmd.Stdout = j.logWriter
			cpCmd.Stderr = j.logWriter

			if err := cpCmd.Run(); err != nil {
				fmt.Fprintf(j.logWriter, "Warning: failed to copy template boot: %v\n", err)
			}
		}
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

	// Настраиваем окружение - наследуем важные переменные хоста
	hostEnv := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=/root",
		"SHELL=/bin/ash",
		"TERM=" + os.Getenv("TERM"),
		"LANG=" + os.Getenv("LANG"),
	}

	// Добавляем переменные из конфигурации
	j.cmd.Env = append(hostEnv, j.config.Environment...)

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

// isMounted проверяет, смонтирован ли указанный путь
func (j *Jail) isMounted(path string) bool {
	// Читаем /proc/mounts для проверки
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		fmt.Fprintf(j.logWriter, "Warning: cannot read /proc/mounts: %v\n", err)
		return false
	}

	// Нормализуем путь
	normalizedPath, err := filepath.Abs(path)
	if err != nil {
		normalizedPath = path
	}

	// Проверяем, есть ли наш путь в списке монтирований
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			mountPoint := fields[1]
			// Сравниваем как точное совпадение, так и нормализованный путь
			if mountPoint == path || mountPoint == normalizedPath {
				return true
			}
		}
	}

	return false
}

// cleanup размонтирует все файловые системы
func (j *Jail) cleanup() {
	fmt.Fprintf(j.logWriter, "Starting cleanup process...\n")

	// Размонтируем в обратном порядке
	for i := len(j.mounts) - 1; i >= 0; i-- {
		mountPoint := j.mounts[i]

		fmt.Fprintf(j.logWriter, "Processing mount point: %s\n", mountPoint)

		// Проверяем, смонтирован ли путь
		if !j.isMounted(mountPoint) {
			fmt.Fprintf(j.logWriter, "Path %s is not mounted, skipping\n", mountPoint)
			continue
		}

		fmt.Fprintf(j.logWriter, "Unmounting %s...\n", mountPoint)

		// Сначала пытаемся обычное размонтирование
		umountCmd := exec.Command("umount", mountPoint)
		umountCmd.Stdout = j.logWriter
		umountCmd.Stderr = j.logWriter

		if err := umountCmd.Run(); err != nil {
			fmt.Fprintf(j.logWriter, "Warning: normal unmount failed for %s: %v\n", mountPoint, err)

			// Принудительное размонтирование
			fmt.Fprintf(j.logWriter, "Trying forced unmount for %s...\n", mountPoint)
			forceCmd := exec.Command("umount", "-f", mountPoint)
			forceCmd.Stdout = j.logWriter
			forceCmd.Stderr = j.logWriter

			if err := forceCmd.Run(); err != nil {
				fmt.Fprintf(j.logWriter, "Warning: forced unmount failed for %s: %v\n", mountPoint, err)

				// Ленивое размонтирование как последний шанс
				fmt.Fprintf(j.logWriter, "Trying lazy unmount for %s...\n", mountPoint)
				lazyCmd := exec.Command("umount", "-l", mountPoint)
				lazyCmd.Stdout = j.logWriter
				lazyCmd.Stderr = j.logWriter
				lazyCmd.Run() // Игнорируем ошибку для lazy unmount
			}
		} else {
			fmt.Fprintf(j.logWriter, "Successfully unmounted %s\n", mountPoint)
		}
	}

	// Очищаем список mount'ов
	j.mounts = []string{}

	// Дополнительная очистка: принудительно размонтируем все что может остаться
	if j.config.ChrootDir != "" {
		fmt.Fprintf(j.logWriter, "Performing additional cleanup for %s...\n", j.config.ChrootDir)

		// Список возможных mount точек для принудительной очистки
		possibleMounts := []string{
			filepath.Join(j.config.ChrootDir, "dev/pts"),
			filepath.Join(j.config.ChrootDir, "dev"),
			filepath.Join(j.config.ChrootDir, "proc"),
			filepath.Join(j.config.ChrootDir, "sys"),
			filepath.Join(j.config.ChrootDir, "template"),
			filepath.Join(j.config.ChrootDir, "scripts"),
			j.config.ChrootDir, // overlay сам по себе
		}

		for _, mount := range possibleMounts {
			if j.isMounted(mount) {
				fmt.Fprintf(j.logWriter, "Found remaining mount: %s, force unmounting...\n", mount)
				exec.Command("umount", "-f", mount).Run()
				exec.Command("umount", "-l", mount).Run()
			}
		}
	}

	// Очищаем loop устройства созданные скриптами (мера безопасности)
	fmt.Fprintf(j.logWriter, "Cleaning up loop devices...\n")
	j.cleanupLoopDevices()

	// Очищаем временные директории
	if strings.Contains(j.config.ChrootDir, "sysweaver") {
		tmpBase := filepath.Dir(j.config.ChrootDir)
		if strings.Contains(tmpBase, "tmp") {
			fmt.Fprintf(j.logWriter, "Removing temporary directory: %s\n", tmpBase)
			os.RemoveAll(tmpBase)
		}
	}

	fmt.Fprintf(j.logWriter, "Cleanup completed\n")
}

// cleanupLoopDevices очищает все loop устройства связанные с образами
func (j *Jail) cleanupLoopDevices() {
	// Получаем список всех loop устройств
	cmd := exec.Command("losetup", "-a")
	output, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(j.logWriter, "Warning: could not list loop devices: %v\n", err)
		return
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}

		// Ищем строки содержащие наши образы
		if strings.Contains(line, "alpine-custom.img") ||
			strings.Contains(line, "sysweaver") {
			// Извлекаем имя loop устройства
			fields := strings.Split(line, ":")
			if len(fields) > 0 {
				loopDev := strings.TrimSpace(fields[0])
				fmt.Fprintf(j.logWriter, "Detaching loop device: %s\n", loopDev)

				// Отключаем loop устройство
				detachCmd := exec.Command("losetup", "-d", loopDev)
				detachCmd.Stdout = j.logWriter
				detachCmd.Stderr = j.logWriter
				detachCmd.Run() // Игнорируем ошибки
			}
		}
	}
}

// Stop останавливает изолированную среду
func (j *Jail) Stop() error {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	if !j.running {
		return fmt.Errorf("jail is not running")
	}

	// Останавливаем процесс
	if j.cmd != nil && j.cmd.Process != nil {
		if err := j.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill process: %w", err)
		}

		// Ждем завершения
		if err := j.cmd.Wait(); err != nil {
			// Игнорируем ошибку, так как процесс уже убит
			fmt.Fprintf(j.logWriter, "Error waiting for process to exit: %v\n", err)
		}
	}

	// Очищаем монтирование
	j.cleanup()

	j.running = false
	return nil
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

// ExecuteCommand выполняет команду в изолированной среде с live выводом (для verbose режима)
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

	// Настраиваем live вывод через logWriter
	cmd.Stdout = j.logWriter
	cmd.Stderr = j.logWriter

	// Выполняем команду с live выводом
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("command failed: %w", err)
	}

	return nil, nil
}

// ExecuteCommandWithOutput выполняет команду и возвращает вывод (для обычного режима)
func (j *Jail) ExecuteCommandWithOutput(command string, args ...string) ([]byte, error) {
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

	// Выполняем команду и собираем вывод
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("command failed: %w", err)
	}

	return output, nil
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
