package provider

import (
	"context"
	"fmt"
	"github.com/billryan/collections/set"
	"net/netip"
	"strconv"
	"strings"

	"github.com/kube-vip/kube-vip-cloud-provider/pkg/ipam"
	"go4.org/netipx"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	cloudprovider "k8s.io/cloud-provider"

	"k8s.io/klog"
)

const (
	// LoadbalancerIPsAnnotations is for specifying IPs for a loadbalancer
	// use plural for dual stack support in the future
	// Example: kube-vip.io/loadbalancerIPs: 10.1.2.3,fd00::100
	LoadbalancerIPsAnnotations = "kube-vip.io/loadbalancerIPs"
	// ImplementationLabelKey is the label key showing the service is implemented by kube-vip
	ImplementationLabelKey = "implementation"
	// ImplementationLabelValue is the label value showing the service is implemented by kube-vip
	ImplementationLabelValue = "kube-vip"
	// LegacyIpamAddressLabelKey is the legacy label key showing the service is implemented by kube-vip
	LegacyIpamAddressLabelKey = "ipam-address"
)

// kubevipLoadBalancerManager -
type kubevipLoadBalancerManager struct {
	kubeClient     kubernetes.Interface
	namespace      string
	cloudConfigMap string
}

func newLoadBalancer(kubeClient kubernetes.Interface, ns, cm string) cloudprovider.LoadBalancer {
	k := &kubevipLoadBalancerManager{
		kubeClient:     kubeClient,
		namespace:      ns,
		cloudConfigMap: cm,
	}
	return k
}

func (k *kubevipLoadBalancerManager) EnsureLoadBalancer(ctx context.Context, _ string, service *v1.Service, _ []*v1.Node) (lbs *v1.LoadBalancerStatus, err error) {
	return syncLoadBalancer(ctx, k.kubeClient, service, k.cloudConfigMap, k.namespace)
}

func (k *kubevipLoadBalancerManager) UpdateLoadBalancer(ctx context.Context, _ string, service *v1.Service, _ []*v1.Node) (err error) {
	_, err = syncLoadBalancer(ctx, k.kubeClient, service, k.cloudConfigMap, k.namespace)
	return err
}

func (k *kubevipLoadBalancerManager) EnsureLoadBalancerDeleted(ctx context.Context, _ string, service *v1.Service) error {
	return k.deleteLoadBalancer(ctx, service)
}

func (k *kubevipLoadBalancerManager) GetLoadBalancer(_ context.Context, _ string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	if service.Labels[ImplementationLabelKey] == ImplementationLabelValue {
		return &service.Status.LoadBalancer, true, nil
	}
	return nil, false, nil
}

// GetLoadBalancerName returns the name of the load balancer. Implementations must treat the
// *v1.Service parameter as read-only and not modify it.
func (k *kubevipLoadBalancerManager) GetLoadBalancerName(_ context.Context, _ string, service *v1.Service) string {
	return getDefaultLoadBalancerName(service)
}

func getDefaultLoadBalancerName(service *v1.Service) string {
	return cloudprovider.DefaultLoadBalancerName(service)
}

func (k *kubevipLoadBalancerManager) deleteLoadBalancer(_ context.Context, service *v1.Service) error {
	klog.Infof("deleting service '%s' (%s)", service.Name, service.UID)

	return nil
}

func checkLegacyLoadBalancerIPAnnotation(ctx context.Context, kubeClient kubernetes.Interface, service *v1.Service) (*v1.LoadBalancerStatus, error) {
	if service.Spec.LoadBalancerIP != "" {
		if v, ok := service.Annotations[LoadbalancerIPsAnnotations]; !ok || len(v) == 0 {
			klog.Warningf("service.Spec.LoadBalancerIP is defined but annotations '%s' is not, assume it's a legacy service, updates its annotations", LoadbalancerIPsAnnotations)
			// assume it's legacy service, need to update the annotation.
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				recentService, getErr := kubeClient.CoreV1().Services(service.Namespace).Get(ctx, service.Name, metav1.GetOptions{})
				if getErr != nil {
					return getErr
				}
				if recentService.Annotations == nil {
					recentService.Annotations = make(map[string]string)
				}
				recentService.Annotations[LoadbalancerIPsAnnotations] = service.Spec.LoadBalancerIP
				// remove ipam-address label
				delete(recentService.Labels, LegacyIpamAddressLabelKey)

				// Update the actual service with the annotations
				_, updateErr := kubeClient.CoreV1().Services(recentService.Namespace).Update(ctx, recentService, metav1.UpdateOptions{})
				return updateErr
			})
			if err != nil {
				return nil, fmt.Errorf("error updating Service Spec [%s] : %v", service.Name, err)
			}
		}
		return &service.Status.LoadBalancer, nil
	}
	return nil, nil
}

// syncLoadBalancer
// 1. Is this loadBalancer already created, and does it have an address? return status
// 2. Is this a new loadBalancer (with no IP address)
// 2a. Get all existing kube-vip services
// 2b. Get the network configuration for this service (namespace) / (CIDR/Range)
// 2c. Between the two find a free address

func syncLoadBalancer(ctx context.Context, kubeClient kubernetes.Interface, service *v1.Service, cmName, cmNamespace string) (*v1.LoadBalancerStatus, error) {
	// This function reconciles the load balancer state
	klog.Infof("syncing service '%s' (%s)", service.Name, service.UID)

	// The loadBalancer address has already been populated
	if status, err := checkLegacyLoadBalancerIPAnnotation(ctx, kubeClient, service); status != nil || err != nil {
		return status, err
	}

	// Check if the service already got a LoadbalancerIPsAnnotation,
	// if so, check if LoadbalancerIPsAnnotation was created by cloud-controller (ImplementationLabelKey == ImplementationLabelValue)
	if v, ok := service.Annotations[LoadbalancerIPsAnnotations]; ok && len(v) != 0 {
		klog.Infof("service '%s/%s' annotations '%s' is defined but service.Spec.LoadBalancerIP is not. Assume it's not legacy service", service.Namespace, service.Name, LoadbalancerIPsAnnotations)
		// Set Label for service lookups
		if service.Labels == nil || service.Labels[ImplementationLabelKey] != ImplementationLabelValue {
			klog.Infof("service '%s/%s' created with pre-defined ip '%s'", service.Namespace, service.Name, v)
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				recentService, getErr := kubeClient.CoreV1().Services(service.Namespace).Get(ctx, service.Name, metav1.GetOptions{})
				if getErr != nil {
					return getErr
				}
				if recentService.Labels == nil {
					// Just because ..
					recentService.Labels = make(map[string]string)
				}
				recentService.Labels[ImplementationLabelKey] = ImplementationLabelValue
				// Update the actual service with the annotations
				_, updateErr := kubeClient.CoreV1().Services(recentService.Namespace).Update(ctx, recentService, metav1.UpdateOptions{})
				return updateErr
			})
			if err != nil {
				return nil, fmt.Errorf("error updating Service Spec [%s] : %v", service.Name, err)
			}
		}
		return &service.Status.LoadBalancer, nil
	}

	// Get the cloud controller configuration map
	controllerCM, err := getConfigMap(ctx, kubeClient, cmName, cmNamespace)
	if err != nil {
		klog.Errorf("Unable to retrieve kube-vip ipam config from configMap [%s] in %s", cmName, cmNamespace)
		// TODO - determine best course of action, create one if it doesn't exist
		controllerCM, err = createConfigMap(ctx, kubeClient, cmName, cmNamespace)
		if err != nil {
			return nil, err
		}
	}

	// Get ip pool from configmap and determine if it is namespace specific or global
	pool, global, allowShare, err := discoverPool(controllerCM, service.Namespace, cmName)
	if err != nil {
		return nil, err
	}

	// Get all services in this namespace or globally, that have the correct label
	var svcs *v1.ServiceList
	if global {
		svcs, err = kubeClient.CoreV1().Services("").List(ctx, metav1.ListOptions{LabelSelector: getKubevipImplementationLabel()})
		if err != nil {
			return &service.Status.LoadBalancer, err
		}
	} else {
		svcs, err = kubeClient.CoreV1().Services(service.Namespace).List(ctx, metav1.ListOptions{LabelSelector: getKubevipImplementationLabel()})
		if err != nil {
			return &service.Status.LoadBalancer, err
		}
	}

	builder := &netipx.IPSetBuilder{}

	var servicePortMap = map[netip.Addr]*set.Set{}

	// Gather infos about implemented services
	for x := range svcs.Items {
		var svc = svcs.Items[x]
		if ip, ok := svc.Annotations[LoadbalancerIPsAnnotations]; ok {
			addr, err := netip.ParseAddr(ip)
			if err != nil {
				return nil, err
			}

			// Store service port mapping to help decide whether services could share the same IP.
			if allowShare {
				if len(svc.Spec.Ports) != 0 {
					for p := range svc.Spec.Ports {
						var port = svc.Spec.Ports[p].Port

						hashSet, ok := servicePortMap[addr]
						if !ok {
							newHashSet := set.NewHashSet()
							servicePortMap[addr] = &newHashSet
							hashSet = servicePortMap[addr]
						}
						(*hashSet).Add(port)
					}
				} else {
					// special case, if the services does not define ports
					klog.Warningf("Service [%s] does not define ports, consider IP %s non-shareble", svc.Name, addr.String())
					newHashSet := set.NewHashSet(0)
					servicePortMap[addr] = &newHashSet
				}
			}

			// Add to IPSet in case we need to find a new free address
			builder.Add(addr)
		}
	}
	inUseSet, err := builder.IPSet()
	if err != nil {
		return nil, err
	}

	descOrder := getSearchOrder(controllerCM)

	loadBalancerIPs := ""

	if allowShare {
		loadBalancerIPs = discoverSharedVIPs(service, servicePortMap)
	}

	if loadBalancerIPs == "" {
		// If allowedShare is true but no IP could be shared, or allowedShare is false, switch to use IPAM lookup
		loadBalancerIPs, err = discoverVIPs(service.Namespace, pool, inUseSet, descOrder, service.Spec.IPFamilyPolicy, service.Spec.IPFamilies)
		if err != nil {
			return nil, err
		}
	}

	// Update the services with this new address
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		recentService, getErr := kubeClient.CoreV1().Services(service.Namespace).Get(ctx, service.Name, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}

		klog.Infof("Updating service [%s], with load balancer IPAM address(es) [%s]", service.Name, loadBalancerIPs)

		if recentService.Labels == nil {
			// Just because ..
			recentService.Labels = make(map[string]string)
		}
		// Set Label for service lookups
		recentService.Labels[ImplementationLabelKey] = ImplementationLabelValue

		if recentService.Annotations == nil {
			recentService.Annotations = make(map[string]string)
		}
		// use annotation instead of label to support ipv6
		recentService.Annotations[LoadbalancerIPsAnnotations] = loadBalancerIPs

		// this line will be removed once kube-vip can recognize annotations
		// Set IPAM address to Load Balancer Service
		recentService.Spec.LoadBalancerIP = strings.Split(loadBalancerIPs, ",")[0]

		// Update the actual service with the address and the labels
		_, updateErr := kubeClient.CoreV1().Services(recentService.Namespace).Update(ctx, recentService, metav1.UpdateOptions{})
		return updateErr
	})
	if retryErr != nil {
		return nil, fmt.Errorf("error updating Service Spec [%s] : %v", service.Name, retryErr)
	}

	return &service.Status.LoadBalancer, nil
}

func getConfigWithNamespace(cm *v1.ConfigMap, namespace, name string) (value, key string, err error) {
	var ok bool

	key = fmt.Sprintf("%s-%s", name, namespace)

	if value, ok = cm.Data[key]; !ok {
		return "", key, fmt.Errorf("no config for %s", name)
	}

	return value, key, nil
}

func getConfig(cm *v1.ConfigMap, namespace, configMapName, name, what, pool string) (value string, global bool, err error) {
	var key string

	value, key, err = getConfigWithNamespace(cm, namespace, name)
	if err != nil {
		klog.Info(fmt.Errorf("no %s config for namespace [%s] exists in key [%s] configmap [%s]", name, namespace, key, configMapName))
		value, key, err = getConfigWithNamespace(cm, "global", name)
		if err != nil {
			klog.Info(fmt.Errorf("no global %s config exists [%s]", name, key))
		} else {
			klog.Infof("Taking %s from [%s]%s", what, key, pool)
			return value, true, nil
		}
	} else {
		klog.Infof("Taking %s from [%s]%s", what, key, pool)
		return value, false, nil
	}

	return "", false, fmt.Errorf("no config for %s", name)
}

func discoverPool(cm *v1.ConfigMap, namespace, configMapName string) (pool string, global bool, allowShare bool, err error) {
	var cidr, ipRange, allowShareStr string

	// Check for VIP sharing
	allowShareStr, _, err = getConfig(cm, namespace, configMapName, "allow-share", "config", "")
	if err == nil {
		allowShare, _ = strconv.ParseBool(allowShareStr)
	}

	// Find Cidr
	cidr, global, err = getConfig(cm, namespace, configMapName, "cidr", "address", " pool")
	if err == nil {
		return cidr, global, allowShare, nil
	}

	// Find Range
	ipRange, global, err = getConfig(cm, namespace, configMapName, "range", "address", " pool")
	if err == nil {
		return ipRange, global, allowShare, nil
	}

	return "", false, allowShare, fmt.Errorf("no address pools could be found")
}

// Multiplex addresses:
// 1. get all used VipEndpoints (addr and port)
// 2. build usedIpset
// 3. find an IP in usedIps where the requested VipEndpoints are available
//		if found: assign this IP and return. Services without a Ports account for the whole IP
//		if not: find new free IP from Range and assign it

func discoverSharedVIPs(service *v1.Service, servicePortMap map[netip.Addr]*set.Set) (vips string) {
	servicePorts := set.NewHashSet()
	for p := range service.Spec.Ports {
		servicePorts.Add(service.Spec.Ports[p].Port)
	}

	for addr := range servicePortMap {
		portSet := *servicePortMap[addr]
		if portSet.Contains(0) {
			continue
		}
		intersect := servicePorts.Intersection(portSet)
		if intersect.Len() == 0 {
			klog.Infof("Share service [%s] ports %s, with address [%s] ports %s",
				service.Name,
				fmt.Sprint(servicePorts.ToSlice()),
				addr.String(),
				fmt.Sprint(portSet.ToSlice()),
			)
			// All requested ports are free on this IP
			return addr.String()
		}
	}

	return ""
}

func discoverVIPs(
	namespace, pool string, inUseIPSet *netipx.IPSet, descOrder bool,
	ipFamilyPolicy *v1.IPFamilyPolicy, ipFamilies []v1.IPFamily,
) (vips string, err error) {
	var ipv4Pool, ipv6Pool string

	// Check if DHCP is required
	if pool == "0.0.0.0/32" {
		return "0.0.0.0", nil
		// Check if ip pool contains a cidr, if not assume it is a range
	} else if len(pool) == 0 {
		return "", fmt.Errorf("could not discover address: pool is not specified")
	} else if strings.Contains(pool, "/") {
		ipv4Pool, ipv6Pool, err = ipam.SplitCIDRsByIPFamily(pool)
	} else {
		ipv4Pool, ipv6Pool, err = ipam.SplitRangesByIPFamily(pool)
	}
	if err != nil {
		return "", err
	}

	vipBuilder := strings.Builder{}

	// Handle single stack case
	if ipFamilyPolicy == nil || *ipFamilyPolicy == v1.IPFamilyPolicySingleStack {
		ipPool := ipv4Pool
		if len(ipFamilies) == 0 {
			if len(ipv4Pool) == 0 {
				ipPool = ipv6Pool
			}
		} else if ipFamilies[0] == v1.IPv6Protocol {
			ipPool = ipv6Pool
		}
		if len(ipPool) == 0 {
			return "", fmt.Errorf("could not find suitable pool for the IP family of the service")
		}
		return discoverAddress(namespace, ipPool, inUseIPSet, descOrder)
	}

	// Handle dual stack case
	if *ipFamilyPolicy == v1.IPFamilyPolicyRequireDualStack {
		// With RequireDualStack, we want to make sure both pools with both IP
		// families exist
		if len(ipv4Pool) == 0 || len(ipv6Pool) == 0 {
			return "", fmt.Errorf("service requires dual-stack, but the configuration does not have both IPv4 and IPv6 pools listed for the namespace")
		}
	}

	primaryPool := ipv4Pool
	secondaryPool := ipv6Pool
	if len(ipFamilies) > 0 && ipFamilies[0] == v1.IPv6Protocol {
		primaryPool = ipv6Pool
		secondaryPool = ipv4Pool
	}
	// Provide VIPs from both IP families if possible (guaranteed if RequireDualStack)
	var primaryPoolErr, secondaryPoolErr error
	if len(primaryPool) > 0 {
		primaryVip, err := discoverAddress(namespace, primaryPool, inUseIPSet, descOrder)
		if err == nil {
			_, _ = vipBuilder.WriteString(primaryVip)
		} else if _, outOfIPs := err.(*ipam.OutOfIPsError); outOfIPs {
			primaryPoolErr = err
		} else {
			return "", err
		}
	}
	if len(secondaryPool) > 0 {
		secondaryVip, err := discoverAddress(namespace, secondaryPool, inUseIPSet, descOrder)
		if err == nil {
			if vipBuilder.Len() > 0 {
				vipBuilder.WriteByte(',')
			}
			_, _ = vipBuilder.WriteString(secondaryVip)
		} else if _, outOfIPs := err.(*ipam.OutOfIPsError); outOfIPs {
			secondaryPoolErr = err
		} else {
			return "", err
		}
	}
	if *ipFamilyPolicy == v1.IPFamilyPolicyPreferDualStack {
		if primaryPoolErr != nil && secondaryPoolErr != nil {
			return "", fmt.Errorf("could not allocate any IP address for PreferDualStack service: %s", renderErrors(primaryPoolErr, secondaryPoolErr))
		}
		singleError := primaryPoolErr
		if secondaryPoolErr != nil {
			singleError = secondaryPoolErr
		}
		if singleError != nil {
			klog.Warningf("PreferDualStack service will be single-stack because of error: %s", singleError)
		}
	} else if *ipFamilyPolicy == v1.IPFamilyPolicyRequireDualStack {
		if primaryPoolErr != nil || secondaryPoolErr != nil {
			return "", fmt.Errorf("could not allocate required IP addresses for RequireDualStack service: %s", renderErrors(primaryPoolErr, secondaryPoolErr))
		}
	}

	return vipBuilder.String(), nil
}

func discoverAddress(namespace, pool string, inUseIPSet *netipx.IPSet, descOrder bool) (vip string, err error) {
	// Check if DHCP is required
	if pool == "0.0.0.0/32" {
		vip = "0.0.0.0"
		// Check if ip pool contains a cidr, if not assume it is a range
	} else if strings.Contains(pool, "/") {
		vip, err = ipam.FindAvailableHostFromCidr(namespace, pool, inUseIPSet, descOrder)
		if err != nil {
			return "", err
		}
	} else {
		vip, err = ipam.FindAvailableHostFromRange(namespace, pool, inUseIPSet, descOrder)
		if err != nil {
			return "", err
		}
	}

	return vip, err
}

func getKubevipImplementationLabel() string {
	return fmt.Sprintf("%s=%s", ImplementationLabelKey, ImplementationLabelValue)
}

func getSearchOrder(cm *v1.ConfigMap) (descOrder bool) {
	if searchOrder, ok := cm.Data["search-order"]; ok {
		if searchOrder == "desc" {
			return true
		}
	}
	return false
}

func renderErrors(errs ...error) string {
	s := strings.Builder{}
	for _, err := range errs {
		if err != nil {
			s.WriteString(fmt.Sprintf("\n\t- %s", err))
		}
	}
	return s.String()
}
