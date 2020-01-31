package scan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ghodss/yaml"
	apiextensionsclientsetscheme "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/scheme"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/proto/hapi/version"
	aggregatorclientsetscheme "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/scheme"

	"github.ibm.com/certauto/cvscan/pkg/stringset"
)

type Scanner struct {
	config          *rest.Config
	clientset       *kubernetes.Clientset
	resourceClients map[string]*rest.RESTClient
	clusterWideOnly bool
}

func New(config *rest.Config, clusterWideOnly bool) (*Scanner, error) {
	s := &Scanner{
		config:          config,
		clusterWideOnly: clusterWideOnly,
	}

	var err error

	s.clientset, err = kubernetes.NewForConfig(s.config)
	if err != nil {
		return nil, fmt.Errorf("creating kube client: %v", err)
	}

	s.resourceClients, err = s.getAllResources()
	if err != nil {
		return nil, fmt.Errorf("getting resource clients: %v", err)
	}

	return s, nil
}

func (s *Scanner) ListAll(namespace string, opts metav1.ListOptions, outDir string) error {
	err := os.MkdirAll(outDir, os.ModePerm)
	if err != nil {
		return fmt.Errorf("creating output directory: %v", err)
	}

	// kubectl get po -o jsonpath='{range .items[*]}{range .metadata.ownerReferences[*]}{"\""}{.kind}{"\",\n"}{end}{end}' --all-namespaces | sort | uniq
	ownedKindsToSkip := map[string]*stringset.StringSet{
		"Pod": stringset.New(
			"ReplicationController",
			"ReplicaSet",
			"StatefulSet",
			"DaemonSet",
			"Job",
		),
		"ReplicaSet": stringset.New("Deployment"),
		"Job":        stringset.New("CronJob"),
	}

	for name, client := range s.resourceClients {
		l := &unstructured.UnstructuredList{}

		req := client.Get().
			Namespace(namespace).
			Resource(name)
		if len(opts.LabelSelector) > 0 {
			req.Param("labelSelector", opts.LabelSelector)
		}

		err := req.Do().Into(l)

		// Ignore MCM resource errors because they require an additional secret
		// to be provided in the request.
		if err != nil && !errors.IsNotFound(err) && !errors.IsMethodNotSupported(err) && !((errors.IsBadRequest(err) || errors.IsServiceUnavailable(err)) && strings.HasSuffix(client.APIVersion().Group, ".clusterapi.io")) {
			return fmt.Errorf("listing %s: %v", name, err)
		}

		if len(l.Items) == 0 {
			continue
		}

	outer:
		for _, item := range l.Items {
			key := "scanned-" + strings.ToLower(item.GetObjectKind().GroupVersionKind().Kind) + "-" + item.GetNamespace() + "-" + item.GetName() + ".yaml"

			y, err := yaml.Marshal(item.Object)
			if err != nil {
				return fmt.Errorf("creating YAML for %s: %v", key, err)
			}

			kind := item.GetObjectKind().GroupVersionKind().Kind
			if skip, exists := ownedKindsToSkip[kind]; exists {
				for _, o := range item.GetOwnerReferences() {
					if skip.Has(o.Kind) {
						continue outer
					}
				}
			}

			// Skip token Secrets generated by ServiceAccounts.
			if kind == "Secret" && len(item.GetAnnotations()["kubernetes.io/service-account.uid"]) > 0 {
				continue
			}

			// Skip services provisioned by Gluster for PVCs
			if kind == "Service" && len(item.GetLabels()["gluster.kubernetes.io/provisioned-for-pvc"]) > 0 {
				continue
			}

			err = ioutil.WriteFile(filepath.Join(outDir, key), y, os.ModePerm)
			if err != nil {
				return err
			}
		}
	}

	c, err := s.getCapabilities()
	if err != nil {
		return fmt.Errorf("reading cluster capabilities: %v", err)
	}

	var release string
	re := regexp.MustCompile(`release=(.*?)(,|\z)`)
	m := re.FindStringSubmatch(opts.LabelSelector)
	if len(m) > 1 {
		release = m[1]
	}

	caps := struct {
		Capabilities *chartutil.Capabilities `json:"capabilities"`
		Namespace    string                  `json:"namespace"`
		ReleaseName  string                  `json:"releaseName"`
	}{c, namespace, release}

	b, err := json.MarshalIndent(caps, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling capabilities to JSON: %v", err)
	}
	if err := ioutil.WriteFile(filepath.Join(outDir, "caps.json"), b, os.ModePerm); err != nil {
		return err
	}

	return nil
}

func (s *Scanner) getAllResources() (map[string]*rest.RESTClient, error) {
	resources := map[string]*rest.RESTClient{}

	vs, err := s.clientset.Discovery().ServerPreferredResources()
	if err, ok := err.(*discovery.ErrGroupDiscoveryFailed); ok && err != nil {
		for group := range err.Groups {
			fmt.Println("Failed to get server API", group)
		}
	} else if err != nil {
		return nil, fmt.Errorf("fetching server API resources: %v", err)
	}

	// Add extra types to scheme
	aggregatorclientsetscheme.AddToScheme(scheme.Scheme)
	apiextensionsclientsetscheme.AddToScheme(scheme.Scheme)

	skip := stringset.New(
		"componentstatuses",
		"controllerrevisions",
		"events",
		"endpoints",
		"packagemanifests",
	)

	for _, v := range vs {
		gv, err := schema.ParseGroupVersion(v.GroupVersion)
		if err != nil {
			log.Printf("Error parsing group/version %q: %s\n", v.GroupVersion, err)
			continue
		}

		config := *s.config
		config.GroupVersion = &gv
		config.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: scheme.Codecs}
		config.APIPath = "/apis"
		if len(gv.Group) == 0 {
			config.APIPath = "/api"
		}

		c, err := rest.RESTClientFor(&config)
		if err != nil {
			log.Println("creating REST client:", err)
			continue
		}

		for _, resource := range v.APIResources {
			name := resource.Name

			if strings.ContainsRune(name, '/') ||
				skip.Has(name) {
				continue
			}

			if s.clusterWideOnly && resource.Namespaced {
				continue
			}

			skip.Add(name)

			resources[name] = c
		}
	}

	return resources, nil
}

func (s *Scanner) getCapabilities() (*chartutil.Capabilities, error) {
	// Copied from k8s.io/helm/pkg/tiller/release_server.go

	c := &chartutil.Capabilities{}

	sv, err := s.clientset.ServerVersion()
	if err != nil {
		return c, fmt.Errorf("server version: %v", err)
	}
	c.KubeVersion = sv

	groups, err := s.clientset.ServerGroups()
	if err != nil {
		return c, fmt.Errorf("server groups: %v", err)
	}

	if groups.Size() == 0 {
		return c, nil
	}

	versions := metav1.ExtractGroupVersions(groups)
	vs := chartutil.NewVersionSet(versions...)
	c.APIVersions = vs

	// Fail fast if Tiller doesn't exist because `helm version` hangs.
	lsc := exec.Command("helm", "ls")
	err = lsc.Run()
	if err != nil {
		lsc := exec.Command("helm", "ls", "--tls")
		err = lsc.Run()
		if err != nil {
			return c, nil
		}
	}

	cmd := exec.Command("helm", "version", "-s", "--template", "{{.Server.SemVer}}")
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	if err != nil {
		cmd = exec.Command("helm", "version", "--tls", "-s", "--template", "{{.Server.SemVer}}")
		cmd.Stdout = &out
		err = cmd.Run()
		if err != nil {
			return c, fmt.Errorf("tiller version: %v", err)
		}
	}
	c.TillerVersion = &version.Version{
		SemVer: out.String(),
	}

	return c, nil
}
