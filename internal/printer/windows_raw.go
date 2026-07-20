package printer

type documentStarter interface {
	StartDocument(name, datatype string) error
}

func startWindowsRawDocument(p documentStarter, name string) error {
	return p.StartDocument(name, "RAW")
}
