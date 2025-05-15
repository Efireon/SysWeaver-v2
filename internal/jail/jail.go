package chroot

import (
	"io"
	"os"
	"os/exec"
	"sync"

	"sysweaver/internal/structures"
)

type Jail struct {
	cmd        *exec.Cmd
	config     structures.JailConf
	configPath string
	running    bool
	mutex      sync.Mutex
	logWriter  io.Writer
}

func NewJail(configPath string) (*Jail, error) {
	var jailConfig structures.JailConfig

	err := config.LoadConfig(configPath, &jailConfig) // configPath - путь до файла конфигурации(путь до него, как правило, один и тот же, и привязан к структуре шаблона. можно будет указать его конкретно в общей конйигурации шаблона)
	if err != nil {
		return nil, err
	}
	return &Jail{
		config:     jailConfig,
		configPath: configPath,
		running:    false,
		logWriter:  os.Stdout,
	}, nil
}

func (m *Manager) Start() error { // Заглушка
}

func (m *Manager) Stop() error { // Заглушка
}

// IsRunning проверяет, выполнена ли изоляция
func (m *Manager) IsRunning() bool {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	return m.running
}

// SetLogWriter устанавливает writer для вывода логов
func (m *Manager) SetLogWriter(writer io.Writer) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.logWriter = writer
}
