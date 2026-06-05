package graph

// Progress observes a Reconcile run so a caller (e.g. the CLI) can render a
// progress bar. Start is called once with the number of files that need
// (re)indexing this run, then Inc once per file examined, then Done at the end.
// All methods may be no-ops; reconciliation never depends on them.
type Progress interface {
	Start(total int)
	Inc()
	// Phase announces a non-file-counted stage (e.g. resolving references) so a
	// long, tick-less step does not look frozen after the file bar fills.
	Phase(label string)
	Done()
}

// noopProgress is the default when no Progress is supplied.
type noopProgress struct{}

func (noopProgress) Start(int)    {}
func (noopProgress) Inc()         {}
func (noopProgress) Phase(string) {}
func (noopProgress) Done()        {}

// reconcileOptions collects optional Reconcile behavior.
type reconcileOptions struct{ progress Progress }

// ReconcileOption configures Reconcile / ReconcileIfStale.
type ReconcileOption func(*reconcileOptions)

// WithProgress reports per-file indexing progress to p.
func WithProgress(p Progress) ReconcileOption {
	return func(o *reconcileOptions) { o.progress = p }
}

// resolve applies opts and fills in defaults.
func (o *reconcileOptions) resolve(opts []ReconcileOption) {
	for _, opt := range opts {
		opt(o)
	}
	if o.progress == nil {
		o.progress = noopProgress{}
	}
}
