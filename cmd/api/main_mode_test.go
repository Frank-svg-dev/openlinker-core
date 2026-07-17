package main

import "testing"

func TestParseCoreServingMode(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		want  coreServingMode
	}{
		{name: "default", want: coreServingModeFull},
		{name: "explicit full", value: "full", want: coreServingModeFull},
		{name: "attach only", value: "runtime-attach-only", want: coreServingModeRuntimeAttachOnly},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseCoreServingMode(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("parseCoreServingMode(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
	if _, err := parseCoreServingMode("attach"); err == nil {
		t.Fatal("unsupported serving mode must fail closed")
	}
}
