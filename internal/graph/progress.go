package graph

// Progress observes a Reconcile run so a caller (e.g. the CLI) can render a
// progress bar. Start is called once with the number of files that need
// (re)indexing this run, then Inc once per file examined, then Done at the end.
// All methods may be no-ops; reconciliation never depends on them.
type Progress interface {
	Start(total int)
	Inc()
	// Phase announces a non-file-counted stage (e.g. resolving references) so a
	// long, tick-less step does not look frozen after the file bar fills. A full
	// Resolve emits a sequence of sub-phase labels, every one prefixed with
	// "resolving references" (e.g. "resolving references (4/8: name fallback)") so a
	// matcher keyed on that prefix still fires while a timestamped log gains
	// per-stage timings for free.
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
type reconcileOptions struct {
	progress Progress
	force    bool
}

// ReconcileOption configures Reconcile / ReconcileIfStale.
type ReconcileOption func(*reconcileOptions)

// WithProgress reports per-file indexing progress to p.
func WithProgress(p Progress) ReconcileOption {
	return func(o *reconcileOptions) { o.progress = p }
}

// WithForce makes ReconcileIfStale ignore the freshness token and Reconcile
// rebuild the secondary indexes and re-run a full resolve even when no file
// changed — the explicit heal lever for a store damaged outside Recarto's view
// (e.g. an aborted bulk load under an older binary).
func WithForce() ReconcileOption {
	return func(o *reconcileOptions) { o.force = true }
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
