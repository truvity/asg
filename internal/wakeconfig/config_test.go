package wakeconfig_test

import (
	"strings"
	"testing"
	"time"

	"github.com/truvity/asg/internal/wakeconfig"
)

const good = `{"region":"eu-west-1","healthPort":8080,"routes":[
 {"name":"arm","listenPort":9001,"selector":{"tagKey":"role","tagValue":"builder-arm"},"targetPort":1234}]}`

func TestDefaults(t *testing.T) {
	c, err := wakeconfig.Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}

	if c.IdleTimeout.D() != 20*time.Minute || c.WakeTimeout.D() != 180*time.Second || c.ActivityTag != "wake-proxy/last-active" {
		t.Errorf("defaults: %+v", c)
	}
}

func TestRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"unknown field": {strings.Replace(good, `"healthPort"`, `"healthPorts"`, 1), "unknown field"},
		"bad duration":  {strings.Replace(good, `"routes"`, `"idleTimeout":"soon","routes"`, 1), "duration"},
		"port clash":    {strings.Replace(good, `"listenPort":9001`, `"listenPort":8080`, 1), "already used"},
		"no routes":     {`{"region":"eu-west-1","healthPort":8080,"routes":[]}`, "at least one route"},
		"bad region":    {strings.Replace(good, "eu-west-1", "mars", 1), "region"},
		"selector=tag":  {strings.Replace(good, `"tagKey":"role"`, `"tagKey":"wake-proxy/last-active"`, 1), "activity tag"},
		"trailing":      {good + `{}`, "trailing"},
	} {
		if _, err := wakeconfig.Parse([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
}
