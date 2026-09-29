package provider

// Typesafe is the TypeSafe-hosted flavour of the Jev-compatible decision
// service. It inherits the entire wire behaviour from the generic
// Systemone provider (jevcompat SPEC 0.1, POST /v1/systemone) — same path,
// headers, validation — and exists only so configurations can distinguish
// "the hosted TypeSafe API" (type: typesafe) from a self-deployed or
// third-party Jev server (type: systemone) in the providers catalogue.
type Typesafe struct {
	*Systemone
}

// NewTypesafe creates the TypeSafe-hosted decision provider.
func NewTypesafe(cfg *ProviderConfig) *Typesafe {
	return &Typesafe{Systemone: NewSystemone(cfg)}
}

// Name overrides the inherited provider id: this entry is "typesafe".
func (t *Typesafe) Name() string { return TypeTypesafe }
