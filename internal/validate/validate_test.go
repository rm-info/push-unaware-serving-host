package validate

import (
	"reflect"
	"testing"
)

func TestOrigins(t *testing.T) {
	got, err := Origins("https://A.test/, https://a.test\nhttps://b.test:8443", true)
	if err != nil || !reflect.DeepEqual(got, []string{"https://a.test", "https://b.test:8443"}) {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", "*", "http://a.test", "https://a.test/path", "https://u:p@a.test", "a.test", "https://"} {
		if _, err := Origins(bad, true); err == nil {
			t.Errorf("%q accepted (required)", bad)
		}
	}
	if got, err := Origins("", false); err != nil || len(got) != 0 {
		t.Fatal("empty list refused when optional")
	}
	if got, err := Origins("* http://localhost:8080", false); err != nil || len(got) != 2 {
		t.Fatalf("operator origins: %v %v", got, err)
	}
}

func TestFields(t *testing.T) {
	for _, ok := range []string{"ab", "my-app", "app2", "a-b-c"} {
		if AppName(ok) != nil {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"a", "-ab", "My-App", "my_app", "my app", "é-app"} {
		if AppName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := Contact("Dev <dev@a.test>"); err == nil {
		t.Error("display name accepted")
	}
	if c, err := Contact(" dev@a.test "); err != nil || c != "dev@a.test" {
		t.Error(c, err)
	}
	if Password("short") == nil || Password("long enough passphrase") != nil {
		t.Error("password rules")
	}
	if _, err := Text("  x  ", "f", 1, true); err != nil {
		t.Error(err)
	}
	if _, err := Text("éé", "f", 1, false); err == nil {
		t.Error("rune limit")
	}
}
