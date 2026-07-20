package api

import (
	"encoding/json"
	"testing"
)

func TestPrintLabelContract(t *testing.T) {
	if TaskPrintLabel != "print_label" {
		t.Fatalf("TaskPrintLabel = %q", TaskPrintLabel)
	}
	if DeviceTypeLabelPrinter != "label_printer" {
		t.Fatalf("DeviceTypeLabelPrinter = %q", DeviceTypeLabelPrinter)
	}

	var data PrintLabelData
	if err := json.Unmarshal([]byte(`{
		"nomenclatureIds":[11,22],
		"name":false,
		"price":true,
		"barcode":false,
		"widthMm":40,
		"heightMm":30,
		"scale":3
	}`), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.NomenclatureIDs) != 2 || data.NomenclatureIDs[0] != 11 {
		t.Fatalf("NomenclatureIDs = %#v", data.NomenclatureIDs)
	}
	if data.Name == nil || *data.Name || data.Price == nil || !*data.Price {
		t.Fatalf("field flags = name:%v price:%v", data.Name, data.Price)
	}
	if data.WidthMM == nil || *data.WidthMM != 40 || data.HeightMM == nil || *data.HeightMM != 30 ||
		data.Scale == nil || *data.Scale != 3 {
		t.Fatalf("dimensions/scale = %v×%v scale:%v", data.WidthMM, data.HeightMM, data.Scale)
	}
}
