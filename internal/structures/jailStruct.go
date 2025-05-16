package structures

// JailConfig содержит настройки для изолированной среды
type JailConfig struct {
	ChrootDir    string       `yaml:"chroot_dir"`
	Environment  []string     `yaml:"environment"`
	BuilderPath  string       `yaml:"builder_path"`
	TemplatePath string       `yaml:"template_path"`
	MountPoints  []MountPoint `yaml:"mount_points"`
	LogPath      string       `yaml:"log_path"`
}

type MountPoint struct {
	Source      string   `yaml:"source"`
	Destination string   `yaml:"destination"`
	Type        string   `yaml:"type"`
	Options     []string `yaml:"options"`
}

// IDMapping для маппинга UID/GID
type IDMapping struct {
	ContainerID int
	HostID      int
	Size        int
}
