package base

// ResourceRelayURLBuilder represents the implemented native, user-scoped
// resource surface. Administrator raw relay alone does not grant this capability.
type ResourceRelayURLBuilder interface {
	BuildResourceRelayURL(escapedPath, rawQuery string) (string, error)
}
