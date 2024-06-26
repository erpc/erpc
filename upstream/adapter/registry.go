package adapter

type Adapter interface {
	Name() string
	NetworkIds() []string
}

type AdapterRegistry struct {
	adapters map[string]Adapter
}

func NewAdapterRegistry() *AdapterRegistry {
	r := &AdapterRegistry{
		adapters: make(map[string]Adapter),
	}

	r.RegisterAdapter(NewEvmUpstreamAdapter())
	r.RegisterAdapter(NewErpcUpstreamAdapter())
	r.RegisterAdapter(NewAlchemyUpstreamAdapter())

	return r
}

func (a *AdapterRegistry) RegisterAdapter(adapter Adapter) {
	a.adapters[adapter.Name()] = adapter
}