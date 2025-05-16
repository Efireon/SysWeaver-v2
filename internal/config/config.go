package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LoadConfig загружает конфигурацию из YAML-файла в указанную структуру
func LoadConfig(path string, config interface{}) error {
	// Проверяем существование файла
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("config file not found: %s", path)
	}

	// Чтение файла
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("error reading config file: %w", err)
	}

	// Распаковка YAML в структуру
	if err := yaml.Unmarshal(data, config); err != nil {
		return fmt.Errorf("error parsing config file: %w", err)
	}

	return nil
}

// ValidateConfig проверяет валидность загруженной конфигурации
func ValidateConfig(config interface{}) error {
	// Здесь будет логика валидации в зависимости от типа конфигурации
	// Для начала реализуем заглушку
	return nil
}

// LoadTemplateConfig загружает конфигурацию из шаблона
func LoadTemplateConfig(templatePath string) (interface{}, error) {
	configPath := filepath.Join(templatePath, "config.yaml")
	var config map[string]interface{}

	if err := LoadConfig(configPath, &config); err != nil {
		return nil, err
	}

	return config, nil
}
