package nativephp

import (
	"slices"
	"testing"
)

func TestNormalizeDisabledFunctions(t *testing.T) {
	got, err := normalizeDisabledFunctions(" EXEC,system\nexec，\\Example\\FunctionName ")
	want := []string{"exec", "system", `example\functionname`}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("functions = %v, %v; want %v", got, err, want)
	}
	for _, input := range []string{"exec()", "Class::method", "exec\nextension=test.so", "shell-exec", "Ksort"} {
		if _, err := normalizeDisabledFunctions(input); err == nil {
			t.Errorf("accepted invalid function name %q", input)
		}
	}
}

func TestFunctionsFromFPMInfo(t *testing.T) {
	got, err := functionsFromFPMInfo("Core\ndisable_functions => exec,strrev => exec,strrev\n")
	if err != nil || !slices.Equal(got, []string{"exec", "strrev"}) {
		t.Fatalf("parsed functions = %v, %v", got, err)
	}
	got, err = functionsFromFPMInfo("disable_functions => no value => no value\r\n")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty functions = %v, %v", got, err)
	}
	if _, err := functionsFromFPMInfo("unrelated output"); err == nil {
		t.Fatal("expected missing option to be reported")
	}
}
