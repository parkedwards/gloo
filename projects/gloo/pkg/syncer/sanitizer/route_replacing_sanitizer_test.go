package sanitizer

import (
	"context"
	"net/http"

	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cache_v3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/golang/protobuf/ptypes"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	v1 "github.com/solo-io/gloo/projects/gloo/pkg/api/v1"
	"github.com/solo-io/gloo/projects/gloo/pkg/translator"
	"github.com/solo-io/solo-kit/pkg/api/v1/control-plane/util"
	"github.com/solo-io/solo-kit/pkg/api/v1/resources/core"
	"github.com/solo-io/solo-kit/pkg/api/v2/reporter"
)

var _ = Describe("RouteReplacingSanitizer", func() {
	var (
		us = &v1.Upstream{
			Metadata: core.Metadata{
				Name:      "my",
				Namespace: "upstream",
			},
		}
		clusterName = translator.UpstreamToClusterName(us.Metadata.Ref())

		missingCluster = "missing_cluster"

		validRouteSingle = &route.Route{
			Action: &route.Route_Route{
				Route: &route.RouteAction{
					ClusterSpecifier: &route.RouteAction_Cluster{
						Cluster: clusterName,
					},
				},
			},
		}

		validRouteMulti = &route.Route{
			Action: &route.Route_Route{
				Route: &route.RouteAction{
					ClusterSpecifier: &route.RouteAction_WeightedClusters{
						WeightedClusters: &route.WeightedCluster{
							Clusters: []*route.WeightedCluster_ClusterWeight{
								{
									Name: clusterName,
								},
								{
									Name: clusterName,
								},
							},
						},
					},
				},
			},
		}

		missingRouteSingle = &route.Route{
			Action: &route.Route_Route{
				Route: &route.RouteAction{
					ClusterSpecifier: &route.RouteAction_Cluster{
						Cluster: missingCluster,
					},
				},
			},
		}

		fixedRouteSingle = &route.Route{
			Action: &route.Route_Route{
				Route: &route.RouteAction{
					ClusterSpecifier: &route.RouteAction_Cluster{
						Cluster: fallbackClusterName,
					},
				},
			},
		}

		missingRouteMulti = &route.Route{
			Action: &route.Route_Route{
				Route: &route.RouteAction{
					ClusterSpecifier: &route.RouteAction_WeightedClusters{
						WeightedClusters: &route.WeightedCluster{
							Clusters: []*route.WeightedCluster_ClusterWeight{
								{
									Name: clusterName,
								},
								{
									Name: missingCluster,
								},
							},
						},
					},
				},
			},
		}

		fixedRouteMulti = &route.Route{
			Action: &route.Route_Route{
				Route: &route.RouteAction{
					ClusterSpecifier: &route.RouteAction_WeightedClusters{
						WeightedClusters: &route.WeightedCluster{
							Clusters: []*route.WeightedCluster_ClusterWeight{
								{
									Name: clusterName,
								},
								{
									Name: fallbackClusterName,
								},
							},
						},
					},
				},
			},
		}

		invalidCfgPolicy = &v1.GlooOptions_InvalidConfigPolicy{
			ReplaceInvalidRoutes:     true,
			InvalidRouteResponseCode: http.StatusTeapot,
			InvalidRouteResponseBody: "out of coffee T_T",
		}

		routeCfgName = "some dirty routes"

		config = &listener.Filter_TypedConfig{}

		// make Consistent() happy
		listener = &listener.Listener{
			FilterChains: []*listener.FilterChain{{
				Filters: []*listener.Filter{{
					Name:       util.HTTPConnectionManager,
					ConfigType: config,
				}},
			}},
		}
	)
	BeforeEach(func() {
		var err error
		config.TypedConfig, err = ptypes.MarshalAny(&hcm.HttpConnectionManager{
			RouteSpecifier: &hcm.HttpConnectionManager_Rds{
				Rds: &hcm.Rds{
					RouteConfigName: routeCfgName,
				},
			},
		})
		Expect(err).NotTo(HaveOccurred())
	})
	It("replaces routes which point to a missing cluster", func() {
		routeCfg := &route.RouteConfiguration{
			Name: routeCfgName,
			VirtualHosts: []*route.VirtualHost{
				{
					Routes: []*route.Route{
						validRouteSingle,
						missingRouteSingle,
					},
				},
				{
					Routes: []*route.Route{
						missingRouteMulti,
						validRouteMulti,
					},
				},
			},
		}

		expectedRoutes := &route.RouteConfiguration{
			Name: routeCfgName,
			VirtualHosts: []*route.VirtualHost{
				{
					Routes: []*route.Route{
						validRouteSingle,
						fixedRouteSingle,
					},
				},
				{
					Routes: []*route.Route{
						fixedRouteMulti,
						validRouteMulti,
					},
				},
			},
		}

		xdsSnapshot := cache_v3.Snapshot{}
		xdsSnapshot.Resources[types.Route] = cache_v3.NewResources("routes", []types.Resource{routeCfg})
		xdsSnapshot.Resources[types.Listener] = cache_v3.NewResources("listeners", []types.Resource{listener})


		sanitizer, err := NewRouteReplacingSanitizer(invalidCfgPolicy)
		Expect(err).NotTo(HaveOccurred())

		// should have a warning to trigger this sanitizer
		reports := reporter.ResourceReports{
			&v1.Proxy{}: {
				Warnings: []string{"route with missing upstream"},
			},
		}

		glooSnapshot := &v1.ApiSnapshot{
			Upstreams: v1.UpstreamList{us},
		}

		snap, err := sanitizer.SanitizeSnapshot(context.TODO(), glooSnapshot, xdsSnapshot, reports)
		Expect(err).NotTo(HaveOccurred())

		routeCfgs := snap.Resources[types.Route]
		listeners := snap.Resources[types.Listener]
		clusters := snap.Resources[types.Cluster]

		sanitizedRoutes := routeCfgs.Items[routeCfg.GetName()]
		listenersWithFallback := listeners.Items[fallbackListenerName]
		clustersWithFallback := clusters.Items[fallbackClusterName]

		Expect(sanitizedRoutes).To(Equal(expectedRoutes))
		Expect(listenersWithFallback).To(Equal(sanitizer.fallbackListener))
		Expect(clustersWithFallback).To(Equal(sanitizer.fallbackCluster))
	})
})
