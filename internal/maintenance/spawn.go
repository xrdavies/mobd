package maintenance

// Helper is how the serving mobd starts `mobd maintain --operation ID`: as its own job of the
// service manager, so stopping the service does not stop the helper. PATH and the working directory
// are the service's, so the restarted service resolves agents and folders as before.
type Helper struct {
	Exe, Dir, Path, Cwd string
}

func (h Helper) args(id string) []string { return []string{h.Exe, "maintain", "--operation", id} }
