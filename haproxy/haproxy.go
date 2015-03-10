package haproxy

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"text/template"
	"time"

	"github.com/newrelic/bosun/service"
	"github.com/newrelic/bosun/services_state"
)

type portset map[string]struct{}
type portmap map[string]portset

// Configuration and state for the HAproxy management module
type HAproxy struct {
	ReloadCmd string
	VerifyCmd string
	BindIP string
	Template string
	ConfigFile string
}

// Constructs a properly configure HAProxy and returns a pointer to it
func New() *HAproxy {
	proxy := HAproxy{
		ReloadCmd: "haproxy -f /etc/haproxy.cfg -p /var/run/haproxy.pid -sf $(cat /var/run/haproxy.pid)",
		VerifyCmd: "haproxy -c /etc/haproxy.cfg",
		Template:  "views/haproxy.cfg",
	}

	return &proxy
}

// Returns a map of sets where each set is a list of ports bound
// by that service.
func (h *HAproxy) makePortmap(services map[string][]*service.Service) portmap {
	ports := make(portmap)

	for name, svcList := range services {
        if _, ok := ports[name]; !ok {
			ports[name] = make(portset, 5)
		}

		for _, service := range svcList {
			for _, svcPort := range service.Ports {
				if svcPort.Type == "tcp" {
					ports[name][strconv.FormatInt(svcPort.Port, 10)] = struct{}{}
				}
			}
		}
	}

	return ports
}

// Clean up image names for writing as HAproxy frontend and backend entries
func sanitizeName(image string) string {
	replace := regexp.MustCompile("[^a-z0-9-]")
	return replace.ReplaceAllString(image, "-")
}

// Create an HAproxy config from the supplied ServicesState. Write it out to the
// supplied io.Writer interface.
func (h *HAproxy) WriteConfig(state *services_state.ServicesState, output io.Writer) {
	services := servicesWithPorts(state)
	ports    := h.makePortmap(services)

	data := struct {
		Services map[string][]*service.Service
	}{
		Services: services,
	}

    funcMap := template.FuncMap{
		"now": time.Now().UTC,
		"getPorts": func(k string) []string {
			var keys []string
			for key, _ := range ports[k] {
				keys = append(keys, key)
			}
			return keys
		},
		"bindIP": func() string { return h.BindIP },
		"sanitizeName": sanitizeName,
    }

	t, err := template.New("haproxy").Funcs(funcMap).ParseFiles(h.Template)
	if err != nil {
		log.Printf("Error Parsing template '%s': %s\n", h.Template, err.Error())
		return
	}
	t.ExecuteTemplate(output, path.Base(h.Template), data)
}

// Execute a command and log the error, but bubble it up as well
func (h *HAproxy) run(command string) error {
	cmd := exec.Command("/bin/bash", "-c", command)
	err := cmd.Run()
	if err != nil {
		log.Printf("Error running '%s': %s", command, err.Error())
	}

	return err
}

// Run the HAproxy reload command to load the new config and restart.
// Best to use a command with -sf specified to keep the connections up.
func (h *HAproxy) Reload() error {
	return h.run(h.ReloadCmd)
}

// Run HAproxy with the verify command that will check the validity of
// the current config. Used to gate a Reload() so we don't load a bad
// config and tear everything down.
func (h *HAproxy) Verify() error {
	return h.run(h.VerifyCmd)
}

// Watch the state of a ServicesState struct and generate a new proxy
// config file (haproxy.ConfigFile) when the state changes. Also notifies
// the service that it needs to reload once the new file has been written
// and verified.
func (h *HAproxy) Watch(state *services_state.ServicesState) {
	lastChange := time.Unix(0, 0)

	for {
		if state.LastChanged.After(lastChange) {
			lastChange = state.LastChanged
			outfile, err := os.Create(h.ConfigFile)
			if err != nil {
				log.Printf("Error: unable to write to %s! (%s)", h.ConfigFile, err.Error())
			}
			h.WriteConfig(state, outfile)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Like state.ByService() but only stores information for services which
// actually have public ports.
func servicesWithPorts(state *services_state.ServicesState) map[string][]*service.Service {
	serviceMap := make(map[string][]*service.Service)

	state.EachServiceSorted(
		func(hostname *string, serviceId *string, svc *service.Service) {
			if len(svc.Ports) < 1 {
				return
			}
			svcName := state.ServiceName(svc)
			if _, ok := serviceMap[svcName]; !ok {
				serviceMap[svcName] = make([]*service.Service, 0, 3)
			}
			serviceMap[svcName] = append(serviceMap[svcName], svc)
		},
	)

	return serviceMap
}
