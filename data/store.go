package data

type Store interface {
	Get(key string) (string, error)
	Set(key string, value string) error
	Scan(prefix string) ([]string, error)
	Delete(key string) error
}
