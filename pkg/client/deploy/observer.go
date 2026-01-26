package deploy

// Observer receives span lifecycle events.
type Observer interface {
	OnSpanStart(span *Span)
	OnSpanUpdate(span *Span) // State/action change, counts update
	OnSpanEnd(span *Span)
}

// NoopObserver is an Observer that does nothing.
type NoopObserver struct{}

func (NoopObserver) OnSpanStart(*Span)  {}
func (NoopObserver) OnSpanUpdate(*Span) {}
func (NoopObserver) OnSpanEnd(*Span)    {}

// MultiObserver broadcasts events to multiple observers.
type MultiObserver []Observer

func (m MultiObserver) OnSpanStart(s *Span) {
	for _, o := range m {
		o.OnSpanStart(s)
	}
}

func (m MultiObserver) OnSpanUpdate(s *Span) {
	for _, o := range m {
		o.OnSpanUpdate(s)
	}
}

func (m MultiObserver) OnSpanEnd(s *Span) {
	for _, o := range m {
		o.OnSpanEnd(s)
	}
}

// ObserverFunc adapts a function to the Observer interface.
// Useful for testing or simple logging.
type ObserverFunc func(*Span)

func (f ObserverFunc) OnSpanStart(s *Span)  { f(s) }
func (f ObserverFunc) OnSpanUpdate(s *Span) { f(s) }
func (f ObserverFunc) OnSpanEnd(s *Span)    { f(s) }
