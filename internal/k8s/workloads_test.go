package k8s

import (
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const image = "tenantscaler:dev"

// containers returns a Deployment's pod containers.
func containers(t *testing.T, d *unstructured.Unstructured) []map[string]interface{} {
	t.Helper()
	list, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	if len(list) == 0 {
		t.Fatalf("deployment %s has no containers", d.GetName())
	}
	out := make([]map[string]interface{}, len(list))
	for i, c := range list {
		out[i] = c.(map[string]interface{})
	}
	return out
}

// containerArgs returns the args of a single-container Deployment.
func containerArgs(t *testing.T, d *unstructured.Unstructured) []string {
	t.Helper()
	cs := containers(t, d)
	if len(cs) != 1 {
		t.Fatalf("deployment %s has %d containers, want 1", d.GetName(), len(cs))
	}
	args, _, _ := unstructured.NestedStringSlice(cs[0], "args")
	return args
}

// containerPort resolves a named or numeric port of a Deployment's container.
func containerPort(t *testing.T, d *unstructured.Unstructured, port interface{}) int64 {
	t.Helper()
	if n, ok := port.(int64); ok {
		return n
	}
	for _, c := range containers(t, d) {
		ports, _, _ := unstructured.NestedSlice(c, "ports")
		for _, p := range ports {
			p := p.(map[string]interface{})
			if p["name"] == port {
				return p["containerPort"].(int64)
			}
		}
	}
	t.Fatalf("deployment %s has no container port %v", d.GetName(), port)
	return 0
}

// dockerfileUID returns the numeric UID of the Dockerfile's last USER line.
// It must be numeric: with runAsNonRoot the kubelet refuses to start an
// image whose USER is a name, because it cannot verify it is not root.
func dockerfileUID(t *testing.T) int64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	var user string
	for line := range strings.Lines(string(data)) {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "USER" {
			user = f[1]
		}
	}
	uid, err := strconv.ParseInt(strings.SplitN(user, ":", 2)[0], 10, 64)
	if err != nil || uid == 0 {
		t.Fatalf("Dockerfile USER %q must be a numeric non-root UID", user)
	}
	return uid
}

// checkRestricted reports where a Deployment's pods fall short of the
// "restricted" Pod Security Standard the namespace enforces, and checks that
// they run as the image's UID.
func checkRestricted(t *testing.T, m manifest, uid int64) {
	t.Helper()
	pod, _, _ := unstructured.NestedMap(m.obj.Object, "spec", "template", "spec")
	for _, host := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if pod[host] == true {
			t.Errorf("%v: %s is not allowed", m, host)
		}
	}
	sc, _, _ := unstructured.NestedMap(pod, "securityContext")
	seccomp, _, _ := unstructured.NestedString(sc, "seccompProfile", "type")
	if sc["runAsNonRoot"] != true || sc["runAsUser"] != uid || seccomp != "RuntimeDefault" {
		t.Errorf("%v: pod securityContext %v, want runAsNonRoot, runAsUser %d and seccomp RuntimeDefault", m, sc, uid)
	}
	for _, c := range containers(t, m.obj) {
		csc, _, _ := unstructured.NestedMap(c, "securityContext")
		drop, _, _ := unstructured.NestedStringSlice(csc, "capabilities", "drop")
		add, _, _ := unstructured.NestedStringSlice(csc, "capabilities", "add")
		if csc["allowPrivilegeEscalation"] != false || csc["privileged"] == true || !slices.Contains(drop, "ALL") || len(add) > 0 {
			t.Errorf("%v: container %v securityContext %v, want no privilege escalation and all capabilities dropped", m, c["name"], csc)
		}
	}
}

func TestPodsMeetRestrictedPodSecurity(t *testing.T) {
	ms := loadManifests(t)
	ns := find(t, ms, "Namespace", testNamespace)
	if ns.GetLabels()["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Errorf("namespace labels = %v, want pod-security enforce=restricted", ns.GetLabels())
	}
	uid := dockerfileUID(t)
	for _, m := range byKind(ms, "Deployment") {
		checkRestricted(t, m, uid)
	}
}

func TestDeploymentsRunTheImageWithExpectedArgs(t *testing.T) {
	ms := deployed(loadManifests(t))
	wantArgs := map[string][]string{
		"model-pool": {"replica", "-addr", ":8000"},
		"gateway": {"gateway", "-addr", ":8080", "-config", "/etc/tenantscaler/config.yaml", "-mode", "fair",
			"-namespace", testNamespace, "-k8s-service", "model-pool", "-k8s-port", "8000"},
		// -decisions-file "": the root filesystem is read-only.
		"tenantscaler-controller": {"controller", "-addr", ":8090", "-k8s", "-namespace", testNamespace,
			"-deployment", "model-pool", "-pool", "shared-llm-pool", "-capacity", "10",
			"-gateway", "http://gateway:8080", "-decisions-file", ""},
	}
	deployments := byKind(ms, "Deployment")
	if len(deployments) != len(wantArgs) {
		t.Errorf("got %d Deployments, want %d", len(deployments), len(wantArgs))
	}
	for _, m := range deployments {
		d := m.obj
		selector, _, _ := unstructured.NestedStringMap(d.Object, "spec", "selector", "matchLabels")
		labels, _, _ := unstructured.NestedStringMap(d.Object, "spec", "template", "metadata", "labels")
		if len(selector) == 0 {
			t.Errorf("%v: empty selector", m)
		}
		for k, v := range selector {
			if labels[k] != v {
				t.Errorf("%v: selector %s=%s does not match the pod template labels", m, k, v)
			}
		}
		for _, c := range containers(t, d) {
			if c["image"] != image {
				t.Errorf("%v: image %v, want %s", m, c["image"], image)
			}
		}
		if got := containerArgs(t, d); !slices.Equal(got, wantArgs[d.GetName()]) {
			t.Errorf("%v: args %q, want %q", m, got, wantArgs[d.GetName()])
		}
		// Every component serves /metrics on its http port.
		annotations, _, _ := unstructured.NestedStringMap(d.Object, "spec", "template", "metadata", "annotations")
		httpPort := strconv.FormatInt(containerPort(t, d, "http"), 10)
		if annotations["prometheus.io/scrape"] != "true" || annotations["prometheus.io/port"] != httpPort ||
			annotations["prometheus.io/path"] != "/metrics" {
			t.Errorf("%v: scrape annotations = %v, want /metrics on port %s", m, annotations, httpPort)
		}
	}

	pool := find(t, ms, "Deployment", "model-pool")
	if _, set, _ := unstructured.NestedFieldNoCopy(pool.Object, "spec", "replicas"); set {
		t.Error("model-pool must not set spec.replicas: the controller owns the replica count")
	}
	replica := containers(t, pool)[0]
	for probe, path := range map[string]string{"readinessProbe": "/health/ready", "livenessProbe": "/health/live"} {
		get, _, _ := unstructured.NestedMap(replica, probe, "httpGet")
		if get["path"] != path || containerPort(t, pool, get["port"]) != 8000 {
			t.Errorf("model-pool %s = %v, want GET %s on 8000", probe, get, path)
		}
	}
	// Graceful scale-down: the grace period must cover the preStop sleep,
	// the replica's drain (replicaDrainTimeout, 30 s in cmd/tenantscaler)
	// and its 5 s HTTP shutdown.
	grace, _, _ := unstructured.NestedInt64(pool.Object, "spec", "template", "spec", "terminationGracePeriodSeconds")
	sleep, _, _ := unstructured.NestedInt64(replica, "lifecycle", "preStop", "sleep", "seconds")
	if sleep < 1 || grace < sleep+30+5 {
		t.Errorf("model-pool preStop sleep %d s, grace period %d s: want a sleep and a grace period >= sleep + 35 s", sleep, grace)
	}

	for _, name := range []string{"gateway", "tenantscaler-controller"} {
		d := find(t, ms, "Deployment", name)
		c := containers(t, d)[0]
		for _, probe := range []string{"readinessProbe", "livenessProbe"} {
			get, _, _ := unstructured.NestedMap(c, probe, "httpGet")
			if get["path"] != "/healthz" || containerPort(t, d, get["port"]) != containerPort(t, d, "http") {
				t.Errorf("%s %s = %v, want GET /healthz on the http port", name, probe, get)
			}
		}
	}
}

func TestServicesMatchDeploymentsAndFlags(t *testing.T) {
	ms := deployed(loadManifests(t))
	servicePort := map[string]int64{}
	for _, m := range byKind(ms, "Service") {
		svc := m.obj
		d := find(t, ms, "Deployment", svc.GetName())
		selector, _, _ := unstructured.NestedStringMap(svc.Object, "spec", "selector")
		labels, _, _ := unstructured.NestedStringMap(d.Object, "spec", "template", "metadata", "labels")
		for k, v := range selector {
			if labels[k] != v {
				t.Errorf("%v: selector %s=%s does not select deployment %s", m, k, v, d.GetName())
			}
		}
		ports, _, _ := unstructured.NestedSlice(svc.Object, "spec", "ports")
		if len(ports) != 1 {
			t.Fatalf("%v: %d ports, want 1", m, len(ports))
		}
		p := ports[0].(map[string]interface{})
		target := containerPort(t, d, p["targetPort"])
		if target != p["port"] {
			t.Errorf("%v: port %v forwards to container port %d", m, p["port"], target)
		}
		// The server must listen on every interface, at the target port.
		addr := flagValues(containerArgs(t, d))["-addr"]
		if host, port, err := net.SplitHostPort(addr); err != nil || host != "" || port != strconv.FormatInt(target, 10) {
			t.Errorf("%v: deployment %s -addr %q, want \":%d\"", m, d.GetName(), addr, target)
		}
		servicePort[svc.GetName()] = p["port"].(int64)
	}
	want := map[string]int64{"model-pool": 8000, "gateway": 8080, "tenantscaler-controller": 8090}
	if !maps.Equal(servicePort, want) {
		t.Errorf("service ports = %v, want %v", servicePort, want)
	}

	gw := flagValues(containerArgs(t, find(t, ms, "Deployment", "gateway")))
	if port, ok := servicePort[gw["-k8s-service"]]; !ok || strconv.FormatInt(port, 10) != gw["-k8s-port"] {
		t.Errorf("gateway -k8s-service %q -k8s-port %q do not match a Service", gw["-k8s-service"], gw["-k8s-port"])
	}
	ctl := flagValues(containerArgs(t, find(t, ms, "Deployment", "tenantscaler-controller")))
	find(t, ms, "Deployment", ctl["-deployment"])
	// Both components must look in the namespace their Roles cover.
	for name, flags := range map[string]map[string]string{"gateway": gw, "controller": ctl} {
		if flags["-namespace"] != testNamespace {
			t.Errorf("%s -namespace = %q, want %q", name, flags["-namespace"], testNamespace)
		}
	}
	u, err := url.Parse(ctl["-gateway"])
	if err != nil || strconv.FormatInt(servicePort[u.Hostname()], 10) != u.Port() {
		t.Errorf("controller -gateway %q does not address a Service and its port (%v)", ctl["-gateway"], err)
	}
}

// grants reports whether a Role rule set allows verb on group/resource.
func grants(rules []interface{}, group, resource, verb string) bool {
	for _, r := range rules {
		r := r.(map[string]interface{})
		groups, _, _ := unstructured.NestedStringSlice(r, "apiGroups")
		resources, _, _ := unstructured.NestedStringSlice(r, "resources")
		verbs, _, _ := unstructured.NestedStringSlice(r, "verbs")
		if slices.Contains(groups, group) && slices.Contains(resources, resource) && slices.Contains(verbs, verb) {
			return true
		}
	}
	return false
}

// roleFor returns the rules of the Role bound to a ServiceAccount.
func roleFor(t *testing.T, ms []manifest, serviceAccount string) []interface{} {
	t.Helper()
	for _, m := range byKind(ms, "RoleBinding") {
		subjects, _, _ := unstructured.NestedSlice(m.obj.Object, "subjects")
		for _, s := range subjects {
			s := s.(map[string]interface{})
			if s["kind"] == "ServiceAccount" && s["name"] == serviceAccount {
				roleName, _, _ := unstructured.NestedString(m.obj.Object, "roleRef", "name")
				rules, _, _ := unstructured.NestedSlice(find(t, ms, "Role", roleName).Object, "rules")
				return rules
			}
		}
	}
	t.Fatalf("no RoleBinding for ServiceAccount %s", serviceAccount)
	return nil
}

func TestRBACCoversWhatTheCodeCalls(t *testing.T) {
	ms := deployed(loadManifests(t))
	for deployment, sa := range map[string]string{"tenantscaler-controller": "tenantscaler-controller", "gateway": "gateway"} {
		got, _, _ := unstructured.NestedString(find(t, ms, "Deployment", deployment).Object, "spec", "template", "spec", "serviceAccountName")
		if got != sa {
			t.Errorf("deployment %s runs as %q, want %q", deployment, got, sa)
		}
		find(t, ms, "ServiceAccount", sa)
	}
	need := []struct {
		sa, group, resource string
		verbs               []string
	}{
		{"tenantscaler-controller", Group, Resource, []string{"get", "list", "watch"}},
		{"tenantscaler-controller", Group, Resource + "/status", []string{"get", "update", "patch"}},
		{"tenantscaler-controller", "apps", "deployments", []string{"get", "list"}},
		{"tenantscaler-controller", "apps", "deployments/scale", []string{"get", "update", "patch"}},
		{"tenantscaler-controller", "discovery.k8s.io", "endpointslices", []string{"get", "list", "watch"}},
		{"gateway", "discovery.k8s.io", "endpointslices", []string{"get", "list", "watch"}},
		{"gateway", Group, Resource, []string{"get", "list", "watch"}},
	}
	for _, n := range need {
		rules := roleFor(t, ms, n.sa)
		for _, verb := range n.verbs {
			if !grants(rules, n.group, n.resource, verb) {
				t.Errorf("%s cannot %s %s.%s", n.sa, verb, n.resource, n.group)
			}
		}
	}
	if rules := roleFor(t, ms, "gateway"); grants(rules, "apps", "deployments/scale", "update") || grants(rules, Group, Resource+"/status", "update") {
		t.Error("the gateway must not be able to scale the pool or write tenant status")
	}
}
