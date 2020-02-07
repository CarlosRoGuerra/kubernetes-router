// Copyright 2017 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package kubernetes

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/tsuru/kubernetes-router/router"
	tsuruv1 "github.com/tsuru/tsuru/provision/kubernetes/pkg/apis/tsuru/v1"
	v1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// defaultLBPort is the default exposed port to the LB
	defaultLBPort = 80

	// exposeAllPortsOpt is the flag used to expose all ports in the LB
	exposeAllPortsOpt = "expose-all-ports"
)

var (
	// ErrLoadBalancerNotReady is returned when a given LB has no IP
	ErrLoadBalancerNotReady = errors.New("load balancer is not ready")
)

// LBService manages LoadBalancer services
type LBService struct {
	*BaseService

	// OptsAsLabels maps router additional options to labels to be set on the service
	OptsAsLabels map[string]string

	// OptsAsLabelsDocs maps router additional options to user friendly help text
	OptsAsLabelsDocs map[string]string

	// PoolLabels maps router additional options for a given pool to be set on the service
	PoolLabels map[string]map[string]string
}

// Create creates a LoadBalancer type service without any selectors
func (s *LBService) Create(appName string, opts router.Opts) error {
	return s.syncLB(appName, &opts, false)
}

// Remove removes the LoadBalancer service
func (s *LBService) Remove(appName string) error {
	client, err := s.getClient()
	if err != nil {
		return err
	}
	service, err := s.getLBService(appName)
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if dstApp, swapped := s.BaseService.isSwapped(service.ObjectMeta); swapped {
		return ErrAppSwapped{App: appName, DstApp: dstApp}
	}
	ns, err := s.getAppNamespace(appName)
	if err != nil {
		return err
	}
	err = client.CoreV1().Services(ns).Delete(service.Name, &metav1.DeleteOptions{})
	if k8sErrors.IsNotFound(err) {
		return nil
	}
	return err
}

// Update updates the LoadBalancer service copying the web service
// labels, selectors, annotations and ports
func (s *LBService) Update(appName string) error {
	return s.syncLB(appName, nil, true)
}

// Swap swaps the two LB services selectors
func (s *LBService) Swap(appSrc string, appDst string) error {
	srcServ, err := s.getLBService(appSrc)
	if err != nil {
		return err
	}
	if !isReady(srcServ) {
		return ErrLoadBalancerNotReady
	}
	dstServ, err := s.getLBService(appDst)
	if err != nil {
		return err
	}
	if !isReady(dstServ) {
		return ErrLoadBalancerNotReady
	}
	s.swap(srcServ, dstServ)
	client, err := s.getClient()
	if err != nil {
		return err
	}
	ns, err := s.getAppNamespace(appSrc)
	if err != nil {
		return err
	}
	ns2, err := s.getAppNamespace(appDst)
	if err != nil {
		return err
	}
	if ns != ns2 {
		return fmt.Errorf("unable to swap apps with different namespaces: %v != %v", ns, ns2)
	}
	_, err = client.CoreV1().Services(ns).Update(srcServ)
	if err != nil {
		return err
	}
	_, err = client.CoreV1().Services(ns).Update(dstServ)
	if err != nil {
		s.swap(srcServ, dstServ)
		_, errRollback := client.CoreV1().Services(ns).Update(srcServ)
		if errRollback != nil {
			return fmt.Errorf("failed to rollback swap %v: %v", err, errRollback)
		}
	}
	return err
}

// Get returns the LoadBalancer IP
func (s *LBService) Get(appName string) (map[string]string, error) {
	service, err := s.getLBService(appName)
	if err != nil {
		return nil, err
	}
	var addr string
	lbs := service.Status.LoadBalancer.Ingress
	if len(lbs) != 0 {
		addr = lbs[0].IP
		ports := service.Spec.Ports
		if len(ports) != 0 {
			addr = fmt.Sprintf("%s:%d", addr, ports[0].Port)
		}
		if lbs[0].Hostname != "" {
			addr = lbs[0].Hostname
		}
	}
	return map[string]string{"address": addr}, nil
}

// SupportedOptions returns all the supported options
func (s *LBService) SupportedOptions() (map[string]string, error) {
	opts := map[string]string{
		router.ExposedPort: "",
		exposeAllPortsOpt:  "Expose all ports used by application in the Load Balancer. Defaults to false.",
	}
	for k, v := range s.OptsAsLabels {
		opts[k] = v
		if s.OptsAsLabelsDocs[k] != "" {
			opts[k] = s.OptsAsLabelsDocs[k]
		}
	}
	return opts, nil
}

func (s *LBService) getLBService(appName string) (*v1.Service, error) {
	client, err := s.getClient()
	if err != nil {
		return nil, err
	}
	ns, err := s.getAppNamespace(appName)
	if err != nil {
		return nil, err
	}
	return client.CoreV1().Services(ns).Get(serviceName(appName), metav1.GetOptions{})
}

func (s *LBService) swap(srcServ, dstServ *v1.Service) {
	srcServ.Spec.Selector, dstServ.Spec.Selector = dstServ.Spec.Selector, srcServ.Spec.Selector
	s.BaseService.swap(&srcServ.ObjectMeta, &dstServ.ObjectMeta)
}

func serviceName(app string) string {
	return fmt.Sprintf("%s-router-lb", app)
}

func isReady(service *v1.Service) bool {
	if len(service.Status.LoadBalancer.Ingress) == 0 {
		return false
	}
	return service.Status.LoadBalancer.Ingress[0].IP != ""
}

func (s *LBService) syncLB(appName string, opts *router.Opts, isUpdate bool) error {
	app, err := s.getApp(appName)
	if err != nil {
		return err
	}
	lbService, err := s.getLBService(appName)
	if err != nil {
		if !k8sErrors.IsNotFound(err) {
			return err
		}
		ns := s.Namespace
		if app != nil {
			ns = app.Spec.NamespaceName
		}
		lbService = &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName(appName),
				Namespace: ns,
			},
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
		}
	}
	if _, isSwapped := s.isSwapped(lbService.ObjectMeta); isSwapped {
		return nil
	}

	if opts == nil {
		var annotationOpts router.Opts
		annotationOpts, err = router.OptsFromAnnotations(&lbService.ObjectMeta)
		if err != nil {
			return err
		}
		opts = &annotationOpts
	}

	webService, err := s.getWebService(appName)
	if err != nil {
		if _, isNotFound := err.(ErrNoService); isUpdate || !isNotFound {
			return err
		}
	}
	if webService != nil {
		lbService.Spec.Selector = webService.Spec.Selector
	}

	err = s.fillLabelsAndAnnotations(lbService, appName, webService, *opts)
	if err != nil {
		return err
	}

	ports, err := s.portsForService(lbService, app, *opts, webService)
	if err != nil {
		return err
	}
	lbService.Spec.Ports = ports

	client, err := s.getClient()
	if err != nil {
		return err
	}
	_, err = client.CoreV1().Services(lbService.Namespace).Update(lbService)
	if k8sErrors.IsNotFound(err) {
		_, err = client.CoreV1().Services(lbService.Namespace).Create(lbService)
	}
	return err
}

func (s *LBService) fillLabelsAndAnnotations(svc *v1.Service, appName string, webService *v1.Service, opts router.Opts) error {
	optsLabels := make(map[string]string)
	for optName, labelName := range s.OptsAsLabels {
		if optsValue, ok := opts.AdditionalOpts[optName]; ok {
			optsLabels[labelName] = optsValue
		}
	}

	labels := []map[string]string{
		s.PoolLabels[opts.Pool],
		optsLabels,
		s.Labels,
		{
			appLabel:            appName,
			managedServiceLabel: "true",
			appPoolLabel:        opts.Pool,
		},
	}
	optsAnnotations, err := opts.ToAnnotations()
	if err != nil {
		return err
	}
	annotations := []map[string]string{s.Annotations, optsAnnotations}

	if webService != nil {
		labels = append(labels, webService.Labels)
		annotations = append(annotations, webService.Annotations)
	}

	svc.Labels = mergeMaps(labels...)
	svc.Annotations = mergeMaps(annotations...)
	return nil
}

func (s *LBService) portsForService(svc *v1.Service, app *tsuruv1.App, opts router.Opts, baseSvc *v1.Service) ([]v1.ServicePort, error) {
	additionalPort, _ := strconv.Atoi(opts.ExposedPort)
	if additionalPort == 0 {
		additionalPort = defaultLBPort
	}

	existingPorts := map[int32]*v1.ServicePort{}
	for i, port := range svc.Spec.Ports {
		existingPorts[port.Port] = &svc.Spec.Ports[i]
	}

	wantedPorts := map[int32]*v1.ServicePort{
		int32(additionalPort): {
			Name:       fmt.Sprintf("port-%d", additionalPort),
			Protocol:   v1.ProtocolTCP,
			Port:       int32(additionalPort),
			TargetPort: intstr.FromInt(getAppServicePort(app)),
		},
	}

	allPorts, _ := strconv.ParseBool(opts.AdditionalOpts[exposeAllPortsOpt])
	if allPorts && baseSvc != nil {
		basePorts := baseSvc.Spec.Ports
		for i := range basePorts {
			if basePorts[i].Port == int32(additionalPort) {
				// Skipping ports conflicting with additional port
				continue
			}
			basePorts[i].NodePort = 0
			wantedPorts[basePorts[i].Port] = &basePorts[i]
		}
	}

	var ports []v1.ServicePort
	for _, wantedPort := range wantedPorts {
		existingPort, ok := existingPorts[wantedPort.Port]
		if ok {
			wantedPort.NodePort = existingPort.NodePort
		}
		ports = append(ports, *wantedPort)
	}
	sort.Slice(ports, func(i, j int) bool {
		return ports[i].Port < ports[j].Port
	})

	return ports, nil
}

func mergeMaps(entries ...map[string]string) map[string]string {
	result := make(map[string]string)
	for _, entry := range entries {
		for k, v := range entry {
			if _, isSet := result[k]; !isSet {
				result[k] = v
			}
		}
	}
	return result
}
