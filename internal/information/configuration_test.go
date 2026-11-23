package information

import (
	"testing"
	"time"
)

func TestInformationPolicyConfigurationDoesNotLaunchServices(t *testing.T) {
	t.Setenv("GORELAY_INFORMATION_REQUEST_TIMEOUT", "19m")
	t.Setenv("GORELAY_BROWSER_ACTION_TIMEOUT", "45s")
	t.Setenv("GORELAY_BROWSER_BINARY", "C:/configuration-only/not-launched.exe")
	s := NewService(nil)
	if err := s.Configure(); err != nil {
		t.Fatal(err)
	}
	if s.RequestTimeout != 19*time.Minute || s.Browser.ActionTimeout != 45*time.Second {
		t.Fatal("host policy replaced by hard defaults", s.RequestTimeout)
	}
}
