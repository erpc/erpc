package config

import (
	"github.com/spf13/afero"
	"gopkg.in/yaml.v2"
)

type Upstream struct {
	Id           string            `yaml:"id"`
	Architecture string            `yaml:"architecture,omitempty"`
	Endpoint     string            `yaml:"endpoint"`
	Metadata     map[string]string `yaml:"metadata"`
}

type Project struct {
	Id        string     `yaml:"id"`
	Upstreams []Upstream `yaml:"upstreams"`
}

// Config represents the configuration of the application.
type Config struct {
	Server struct {
		HttpHost     string `yaml:"httpHost"`
		HttpPort     string `yaml:"httpPort"`
		maxTimeoutMs int    `yaml:"maxTimeoutMs"`
	} `yaml:"server"`
	LogLevel string    `yaml:"logLevel"`
	Projects []Project `yaml:"projects"`
}

// LoadConfig loads the configuration from the specified file.
func LoadConfig(fs afero.Fs, filename string) (*Config, error) {
	data, err := afero.ReadFile(fs, filename)

	if err != nil {
		return nil, err
	}

	var cfg Config
	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		return nil, err
	}

	return &cfg, nil
}
