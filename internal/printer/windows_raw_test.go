package printer

import "testing"

type recordingDocumentStarter struct {
	name     string
	datatype string
}

func (r *recordingDocumentStarter) StartDocument(name, datatype string) error {
	r.name = name
	r.datatype = datatype
	return nil
}

func TestStartWindowsRawDocumentForcesRAW(t *testing.T) {
	starter := &recordingDocumentStarter{}

	if err := startWindowsRawDocument(starter, "label"); err != nil {
		t.Fatalf("startWindowsRawDocument() error = %v", err)
	}
	if starter.name != "label" || starter.datatype != "RAW" {
		t.Fatalf("StartDocument() = (%q, %q), want (%q, %q)", starter.name, starter.datatype, "label", "RAW")
	}
}
