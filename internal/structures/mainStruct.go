package structures

type BuildConfig struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	Base    struct {
		Distro  string `yaml:"distro"`
		Version string `yaml:"version"`
	} `yaml:"base"`
	System struct {
		Hostname string `yaml:"hostname"`
		Timezone string `yaml:"timezone"`
		Locale   string `yaml:"locale"`
	} `yaml:"system"`
	Partitions []struct {
		Name       string   `yaml:"name"`
		Size       string   `yaml:"size"`
		Filesystem string   `yaml:"filesystem"`
		Mount      string   `yaml:"mount"`
		Flags      []string `yaml:"flags"`
	} `yaml:"partitions"`
	ISO struct {
		Label       string `yaml:"label"`
		Publisher   string `yaml:"publisher"`
		Compression string `yaml:"compression"`
	} `yaml:"iso"`
	Packages []string `yaml:"packages"`
}
