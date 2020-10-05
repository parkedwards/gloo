package consul

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/rotisserie/eris"

	"github.com/solo-io/gloo/projects/gloo/pkg/discovery"

	"github.com/solo-io/gloo/projects/gloo/pkg/upstreams/consul"

	envoyapi "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	v1 "github.com/solo-io/gloo/projects/gloo/pkg/api/v1"
	"github.com/solo-io/gloo/projects/gloo/pkg/plugins"
	"github.com/solo-io/gloo/projects/gloo/pkg/xds"
)

var _ discovery.DiscoveryPlugin = new(plugin)

var (
	DefaultDnsAddress         = "127.0.0.1:8600"
	DefaultDnsPollingInterval = 5 * time.Second
	DefaultTlsTagName         = "glooUseTls"

	ConsulTlsInputError = func(msg string) error {
		return eris.Errorf(msg)

	}
)

type plugin struct {
	client             consul.ConsulWatcher
	resolver           DnsResolver
	dnsPollingInterval time.Duration
	consulSettings     *v1.Settings_ConsulConfiguration
}

func (p *plugin) Resolve(u *v1.Upstream) (*url.URL, error) {
	consulSpec, ok := u.UpstreamType.(*v1.Upstream_Consul)
	if !ok {
		return nil, nil
	}

	spec := consulSpec.Consul

	// default to first datacenter
	var dc string
	if len(spec.DataCenters) > 0 {
		dc = spec.DataCenters[0]
	}

	instances, _, err := p.client.Service(spec.ServiceName, "", &api.QueryOptions{Datacenter: dc, RequireConsistent: true})
	if err != nil {
		return nil, eris.Wrapf(err, "getting service from catalog")
	}

	scheme := "http"
	if u.SslConfig != nil {
		scheme = "https"
	}

	// Match service instances (consul endpoints) to gloo upstreams. A match is found if the upstream's
	// InstanceTags array is a subset of the serviceInstance's tags, or always if InstanceTags is empty.
	// If the upstream's instanceBlackListTags array is non-empty, then there must also be no matches between
	// this and the service instances tags.
	//
	// There's no coordination between upstreams when matching. This makes it a little awkward to sort
	// consul serviceInstance's among upstreams if we have any upstream with an empty InstanceTags array,
	// since that will also auto-match with serviceInstances that had matching tags for another upstream.
	//
	// The resulting implication is:
	// If there are multiple upstreams associated with the same consul service, each upstream MUST have a non-empty
	// InstanceTags array, and that service's serviceInstances MUST have enough tags to match them to at least one
	// service. If a serviceInstance has the tags to match into multiple upstreams, there's no guarantee which it'll
	// be associated with.
	for _, inst := range instances {
		instanceMatch := len(spec.InstanceTags) == 0 || matchTags(spec.InstanceTags, inst.ServiceTags)
		antiInstanceMatch := len(spec.InstanceBlacklistTags) == 0 || mutuallyExclusiveTags(spec.InstanceBlacklistTags, inst.ServiceTags)

		if instanceMatch && antiInstanceMatch {
			ipAddresses, err := getIpAddresses(context.TODO(), inst.ServiceAddress, p.resolver)
			if err != nil {
				return nil, err
			}
			if len(ipAddresses) == 0 {
				return nil, eris.Errorf("DNS result for %s returned an empty list of IPs", inst.ServiceAddress)
			}
			// arbitrarily default to the first result
			ipAddr := ipAddresses[0]
			return url.Parse(fmt.Sprintf("%v://%v:%v", scheme, ipAddr, inst.ServicePort))
		}
	}

	return nil, eris.Errorf("service with name %s and tags %v not found", spec.ServiceName, spec.InstanceTags)
}

func NewPlugin(client consul.ConsulWatcher, resolver DnsResolver, dnsPollingInterval *time.Duration) *plugin {
	pollingInterval := DefaultDnsPollingInterval
	if dnsPollingInterval != nil {
		pollingInterval = *dnsPollingInterval
	}
	return &plugin{client: client, resolver: resolver, dnsPollingInterval: pollingInterval}
}

func (p *plugin) Init(params plugins.InitParams) error {
	p.consulSettings = params.Settings.Consul
	if p.consulSettings == nil {
		p.consulSettings = &v1.Settings_ConsulConfiguration{UseTlsTagging: false}
	}
	// if automatic TLS discovery is enabled for consul services, make sure we have a specified tag
	// and a resource location for the validation context's root CA.
	// The tag has a default value, but the resource name/namespace must be set manually.
	if p.consulSettings != nil && p.consulSettings.UseTlsTagging {
		rootCa := p.consulSettings.GetRootCa()
		if rootCa == nil || rootCa.GetNamespace() == "" || rootCa.GetName() == "" {
			return ConsulTlsInputError(fmt.Sprintf("Consul settings specify automatic detection of TLS services, "+
				"but the rootCA resource's name/namespace are not properly specified: {%s}", rootCa.String()))
		}

		tlsTagName := p.consulSettings.GetTlsTagName()
		if tlsTagName == "" {
			p.consulSettings.TlsTagName = DefaultTlsTagName
		}
	}
	return nil
}

func (p *plugin) ProcessUpstream(params plugins.Params, in *v1.Upstream, out *envoyapi.Cluster) error {
	_, ok := in.UpstreamType.(*v1.Upstream_Consul)
	if !ok {
		return nil
	}

	// consul upstreams use EDS
	xds.SetEdsOnCluster(out)

	return nil
}

// make sure t1 is a subset of t2
func matchTags(t1, t2 []string) bool {
	if len(t1) > len(t2) {
		return false
	}
	for _, tag1 := range t1 {
		var found bool
		for _, tag2 := range t2 {
			if tag1 == tag2 {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// make sure t1 and t2 are mutually exclusive
func mutuallyExclusiveTags(t1, t2 []string) bool {
	for _, tag1 := range t1 {
		var found bool
		for _, tag2 := range t2 {
			if tag1 == tag2 {
				found = true
				break
			}
		}
		if found {
			return false
		}
	}
	return true
}
