package deploy

// Observer receives deployment progress events.
type Observer interface {
	OnEvent(event Event)
}

// NoopObserver is an Observer that does nothing.
type NoopObserver struct{}

func (NoopObserver) OnEvent(Event) {}

// MultiObserver broadcasts events to multiple observers.
type MultiObserver []Observer

func (m MultiObserver) OnEvent(e Event) {
	for _, o := range m {
		o.OnEvent(e)
	}
}

// ObserverFunc adapts a function to the Observer interface.
type ObserverFunc func(Event)

func (f ObserverFunc) OnEvent(e Event) { f(e) }
