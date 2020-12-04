package packet

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/packethost/packet-ccm/packet/metallb"
	"github.com/packethost/packngo"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog"
)

const (
	bufferSize = 4096
)

type loadBalancers struct {
	client             *packngo.Client
	k8sclient          kubernetes.Interface
	project            string
	configmapnamespace string
	configmapname      string
	configmapenable    bool
	facility           string
	localASN, peerASN  int
}

func newLoadBalancers(client *packngo.Client, projectID, facility, configmap string, configmapenable bool, localASN, peerASN int) *loadBalancers {
	// parse the configmap - we only accept a full namespace and name, no fallback defaults
	var configmapnamespace, configmapname string
	cmparts := strings.SplitN(configmap, ":", 2)
	if len(cmparts) >= 2 {
		configmapnamespace, configmapname = cmparts[0], cmparts[1]
	}
	return &loadBalancers{client, nil, projectID, configmapnamespace, configmapname, configmapenable, facility, localASN, peerASN}
}

func (l *loadBalancers) name() string {
	return "loadbalancer"
}

func (l *loadBalancers) init(k8sclient kubernetes.Interface) error {
	klog.V(2).Info("loadBalancers.init(): started")
	l.k8sclient = k8sclient

	if !l.configmapenable {
		klog.V(2).Info("No ConfigMap has been entered to watch, not managing metallb")
	}

	klog.V(2).Info("loadBalancers.init(): complete")
	return nil
}

// implementation of cloudprovider.LoadBalancer
// we do this via metallb, not directly, so none of this works... for now.

func (l *loadBalancers) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	return nil, false, nil
}
func (l *loadBalancers) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	return ""
}
func (l *loadBalancers) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	return nil, nil
}
func (l *loadBalancers) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	return nil
}
func (l *loadBalancers) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	return nil
}

// utility funcs

func (l *loadBalancers) nodeReconciler() nodeReconciler {
	// If the configmap is disabled then the nodes can't be reconciled
	if l.configmapname == "" {
		klog.V(2).Info("loadBalancers disabled, not enabling nodeReconciler")
		return nil
	}
	return l.reconcileNodes
}

func (l *loadBalancers) serviceReconciler() serviceReconciler {
	// If the configmap isn't disabled, but we're not watching a configmap then disable the reconciler
	if l.configmapenable && l.configmapname == "" {
		klog.V(2).Info("loadBalancers disabled, not enabling serviceReconciler")
		return nil
	}
	return l.reconcileServices
}

// reconcileNodes given a node, update the metallb load balancer by
// by adding it to or removing it from the known metallb configmap
func (l *loadBalancers) reconcileNodes(nodes []*v1.Node, mode UpdateMode) error {
	var (
		peers        []string
		err          error
		changedNodes bool
		changed      bool
	)
	klog.V(2).Infof("loadbalancers.reconcileNodes(): called for nodes %v", nodes)

	// get the configmap
	cmInterface := l.k8sclient.CoreV1().ConfigMaps(l.configmapnamespace)

	klog.V(2).Infof("loadbalancers.reconcileNodes(): getting configmap %s/%s", l.configmapnamespace, l.configmapname)
	config, err := getMetalConfigMap(cmInterface, l.configmapname)
	if err != nil {
		return fmt.Errorf("failed to get metallb config map %s:%s : %v", l.configmapnamespace, l.configmapname, err)
	}
	klog.V(5).Infof("loadbalancers.reconcileNodes(): original configmap %#v", config)
	// are we adding, removing or syncing the node?
	switch mode {
	case ModeRemove:
		for _, node := range nodes {
			klog.V(2).Infof("loadbalancers.reconcileNodes(): reconciling remove node %s", node.Name)
			config, changed = removeNodePeer(config, node.Name)
			if !changed {
				klog.V(2).Infof("loadbalancers.reconcileNodes(): config unchanged for remove %s", node.Name)
			} else {
				klog.V(2).Infof("loadbalancers.reconcileNodes(): config changed for remove %s", node.Name)
				changedNodes = true
			}
		}
	case ModeAdd:
		for _, node := range nodes {
			klog.V(2).Infof("loadbalancers.reconcileNodes(): reconciling add node %s", node.Name)
			// get the node provider ID
			id := node.Spec.ProviderID
			if id == "" {
				return fmt.Errorf("no provider ID given for node %s", node.Name)
			}
			if peers, err = getNodePeerAddress(id, l.client); err != nil || len(peers) < 1 {
				klog.Errorf("loadbalancers.reconcileNodes(): could not add metallb node peer address for node %s: %v", node.Name, err)
				continue
			}
			config, changed = addNodePeer(config, node.Name, l.localASN, l.peerASN, peers...)
			if !changed {
				klog.V(2).Infof("loadbalancers.reconcileNodes(): config unchanged for add %s", node.Name)
			} else {
				klog.V(2).Infof("loadbalancers.reconcileNodes(): config changed for add %s", node.Name)
				changedNodes = true
			}
		}
	case ModeSync:
		// make sure the list of nodes exactly matches between the provided nodes and the ones in the configmap
		goodMap := map[string]bool{}
		for _, node := range nodes {
			goodMap[node.Name] = true
		}
		// first remove everything from the configmap that is not in goodMap
		configNodes := getNodes(config)
		for _, node := range configNodes {
			if _, ok := goodMap[node]; !ok {
				klog.V(2).Infof("loadbalancers.reconcileNodes(): removing node from configmap: %s", node)
				config, _ = removeNodePeer(config, node)
				changedNodes = true
			}
		}
		// now see if any nodes are missing
		// get the list of nodes afresh
		configNodes = getNodes(config)
		configMap := map[string]bool{}
		for _, node := range configNodes {
			configMap[node] = true
		}
		for _, node := range nodes {
			if _, ok := configMap[node.Name]; !ok {
				// get the node provider ID
				id := node.Spec.ProviderID
				if id == "" {
					return fmt.Errorf("no provider ID given for node %s", node.Name)
				}
				if peers, err = getNodePeerAddress(id, l.client); err != nil || len(peers) < 1 {
					klog.Errorf("loadbalancers.reconcileNodes(): could not get node peer address for node %s: %v", node.Name, err)
					continue
				}
				config, _ = addNodePeer(config, node.Name, l.localASN, l.peerASN, peers...)
				// we do not need to check if it is changed; we know it is
				changedNodes = true
			}
		}
	}
	if !changedNodes {
		klog.V(2).Info("loadbalancers.reconcileNodes(): no change to configmap, not updating")
		return nil
	}
	klog.V(2).Infof("loadbalancers.reconcileNodes(): config changed, updating to %#v", config)
	return saveUpdatedConfigMap(cmInterface, l.configmapname, config)
}

// reconcileServices add or remove services to have loadbalancers. If it adds a
// service, then it requests a new IP reservation, with "fast-fail", i.e. if it
// cannot create the IP reservation immediately, then it fails, rather than
// waiting for human support. It tags the IP reservation so it can find it later.
// Before trying to create one, it tries to find an IP reservation with the right tags.
func (l *loadBalancers) reconcileServices(svcs []*v1.Service, mode UpdateMode) error {
	klog.V(2).Infof("loadbalancer.reconcileServices(): %v starting", mode)
	klog.V(5).Infof("loadbalancer.reconcileServices(): services %#v", svcs)

	var err error
	// get IP address reservations and check if they any exists for this svc
	ips, _, err := l.client.ProjectIPs.List(l.project, &packngo.ListOptions{})
	if err != nil {
		return fmt.Errorf("unable to retrieve IP reservations for project %s: %v", l.project, err)
	}
	var config *metallb.ConfigFile
	if l.configmapenable {

		// get the configmap
		cmInterface := l.k8sclient.CoreV1().ConfigMaps(l.configmapnamespace)

		config, err = getMetalConfigMap(cmInterface, l.configmapname)
		if err != nil {
			return fmt.Errorf("unable to retrieve metallb config map %s:%s : %v", l.configmapnamespace, l.configmapname, err)
		}
	}

	validSvcs := []*v1.Service{}
	for _, svc := range svcs {
		// filter on type: only take those that are of type=LoadBalancer
		if svc.Spec.Type == v1.ServiceTypeLoadBalancer {
			validSvcs = append(validSvcs, svc)
		}
	}
	klog.V(5).Infof("loadbalancer.reconcileServices(): valid services %#v", validSvcs)

	switch mode {
	case ModeAdd:
		// ADDITION
		for _, svc := range validSvcs {
			klog.V(2).Infof("loadbalancer.reconcileServices(): add: service %s", svc.Name)
			if err := l.addService(svc, ips, config); err != nil {
				return err
			}
		}
	case ModeRemove:
		// REMOVAL
		for _, svc := range validSvcs {
			svcName := serviceRep(svc)
			svcTag := serviceTag(svc)
			svcIP := svc.Spec.LoadBalancerIP

			var svcIPCidr string
			ipReservation := ipReservationByAllTags([]string{svcTag, packetTag}, ips)

			klog.V(2).Infof("loadbalancer.reconcileServices(): remove: %s with existing IP assignment %s", svcName, svcIP)

			// get the IPs and see if there is anything to clean up
			if ipReservation == nil {
				klog.V(2).Infof("loadbalancer.reconcileServices(): remove: no IP reservation found for %s, nothing to delete", svcName)
				continue
			}
			// delete the reservation
			klog.V(2).Infof("loadbalancer.reconcileServices(): remove: for %s EIP ID %s", svcName, ipReservation.ID)
			_, err = l.client.ProjectIPs.Remove(ipReservation.ID)
			if err != nil {
				return fmt.Errorf("failed to remove IP address reservation %s from project: %v", ipReservation.String(), err)
			}
			// remove it from the configmap
			svcIPCidr = fmt.Sprintf("%s/%d", ipReservation.Address, ipReservation.CIDR)
			klog.V(2).Infof("loadbalancer.reconcileServices(): remove: for %s configmap entry %s", svcName, svcIPCidr)
			if err = unmapIP(config, svcIPCidr, l.configmapnamespace, l.configmapname, l.k8sclient); err != nil {
				return fmt.Errorf("error removing IP from configmap for %s: %v", svcName, err)
			}
			klog.V(2).Infof("loadbalancer.reconcileServices(): remove: removed service from configmap %s", svcName)
		}
	case ModeSync:
		// what we have to do:
		// 1. get all of the services that are of type=LoadBalancer
		// 2. for each service, get its eip, if available. if it does not have one, create one for it.
		// 3. for each EIP, ensure it exists in the configmap
		// 4. get each EIP in the configmap, check if it is in our list; if not, delete

		// add each service that is in the known list
		for _, svc := range validSvcs {
			klog.V(2).Infof("loadbalancer.reconcileServices(): sync: service %s", svc.Name)
			if err := l.addService(svc, ips, config); err != nil {
				return err
			}
		}

		// remove any service that is not in the known list

		// we need to get the addresses again, because we might have changed them
		klog.V(5).Info("loadbalancer.reconcileServices(): sync: getting all IP reservations")
		ips, _, err = l.client.ProjectIPs.List(l.project, &packngo.ListOptions{})
		if err != nil {
			return fmt.Errorf("unable to retrieve IP reservations for project %s: %v", l.project, err)
		}
		// get all EIP that have the packet tag
		ipReservations := ipReservationsByAnyTags([]string{packetTag}, ips)
		// create a map of EIP to svcIP so we can get the CIDR
		ipCidr := map[string]int{}
		for _, ipr := range ipReservations {
			ipCidr[ipr.Address] = ipr.CIDR
		}

		// create a map of all valid IPs
		validTags := map[string]bool{}
		validIPs := map[string]bool{}

		for _, svc := range validSvcs {
			validTags[serviceTag(svc)] = true
			svcIP := svc.Spec.LoadBalancerIP
			if svcIP != "" {
				if cidr, ok := ipCidr[svcIP]; ok {
					validIPs[fmt.Sprintf("%s/%d", svcIP, cidr)] = true
				}
			}
		}

		klog.V(2).Infof("loadbalancer.reconcileServices(): sync: valid tags %v", validTags)
		klog.V(2).Infof("loadbalancer.reconcileServices(): sync: valid svc IPs %v", validIPs)
		if config != nil {
			// get all IPs registered in the configmap; remove those not in our valid list
			configIPs := getServiceAddresses(config)
			klog.V(2).Infof("loadbalancer.reconcileServices(): sync: actual configmap IPs %v", configIPs)
			for _, ip := range configIPs {
				if _, ok := validIPs[ip]; !ok {
					klog.V(2).Infof("loadbalancer.reconcileServices(): sync: removing from configmap ip %s not in valid list", ip)
					if err = unmapIP(config, ip, l.configmapnamespace, l.configmapname, l.k8sclient); err != nil {
						return fmt.Errorf("error removing IP from configmap %s: %v", ip, err)
					}
				}
			}
		}

		// remove any EIPs that do not have a reservation

		klog.V(5).Infof("loadbalancer.reconcileServices(): sync: all reservations with packetTag %#v", ipReservations)
		for _, ipReservation := range ipReservations {
			var foundTag bool
			for _, tag := range ipReservation.Tags {
				if _, ok := validTags[tag]; ok {
					foundTag = true
				}
			}
			// did we find a valid tag?
			if !foundTag {
				klog.V(2).Infof("loadbalancer.reconcileServices(): sync: removing reservation with service= tag but not in validTags list %#v", ipReservation)
				// delete the reservation
				_, err = l.client.ProjectIPs.Remove(ipReservation.ID)
				if err != nil {
					return fmt.Errorf("failed to remove IP address reservation %s from project: %v", ipReservation.String(), err)
				}
			}
		}
	}
	return nil
}

func (l *loadBalancers) addService(svc *v1.Service, ips []packngo.IPAddressReservation, config *metallb.ConfigFile) error {
	svcName := serviceRep(svc)
	svcTag := serviceTag(svc)
	svcIP := svc.Spec.LoadBalancerIP

	var (
		svcIPCidr string
		err       error
	)
	ipReservation := ipReservationByAllTags([]string{svcTag, packetTag}, ips)

	klog.V(2).Infof("processing %s with existing IP assignment %s", svcName, svcIP)
	// if it already has an IP, no need to get it one
	if svcIP == "" {
		klog.V(2).Infof("no IP assigned for service %s; searching reservations", svcName)

		// if no IP found, request a new one
		if ipReservation == nil {

			// if we did not find an IP reserved, create a request
			klog.V(2).Infof("no IP assignment found for %s, requesting", svcName)
			// create a request
			facility := l.facility
			req := packngo.IPReservationRequest{
				Type:        "public_ipv4",
				Quantity:    1,
				Description: ccmIPDescription,
				Facility:    &facility,
				Tags: []string{
					packetTag,
					svcTag,
				},
				FailOnApprovalRequired: true,
			}

			ipReservation, _, err = l.client.ProjectIPs.Request(l.project, &req)
			if err != nil {
				return fmt.Errorf("failed to request an IP for the load balancer: %v", err)
			}
		}

		// if we have no IP from existing or a new reservation, log it and return
		if ipReservation == nil {
			klog.V(2).Infof("no IP to assign to service %s, will need to wait until it is allocated", svcName)
			return nil
		}

		// we have an IP, either found from existing reservations or a new reservation.
		// map and assign it
		svcIP = ipReservation.Address

		// assign the IP and save it
		klog.V(2).Infof("assigning IP %s to %s", svcIP, svcName)
		intf := l.k8sclient.CoreV1().Services(svc.Namespace)
		existing, err := intf.Get(svc.Name, metav1.GetOptions{})
		if err != nil || existing == nil {
			klog.V(2).Infof("failed to get latest for service %s: %v", svcName, err)
			return fmt.Errorf("failed to get latest for service %s: %v", svcName, err)
		}
		existing.Spec.LoadBalancerIP = svcIP

		_, err = intf.Update(existing)
		if err != nil {
			klog.V(2).Infof("failed to update service %s: %v", svcName, err)
			return fmt.Errorf("failed to update service %s: %v", svcName, err)
		}

	}
	// our default CIDR for each address is 32
	cidr := 32
	if ipReservation != nil {
		cidr = ipReservation.CIDR
	}
	svcIPCidr = fmt.Sprintf("%s/%d", svcIP, cidr)
	if config != nil {
		// Update the service and configmap and save them
		if err = mapIP(config, svcIPCidr, svcName, l.configmapnamespace, l.configmapname, l.k8sclient); err != nil {
			return fmt.Errorf("error mapping IP %s: %v", svcName, err)
		}
	}
	return nil
}

// getNodes get the names of nodes in the metallb configmap
func getNodes(config *metallb.ConfigFile) []string {
	nodes := []string{}
	peers := config.Peers
	for _, p := range peers {
		for _, selector := range p.NodeSelectors {
			for k, v := range selector.MatchLabels {
				if k == hostnameKey {
					nodes = append(nodes, v)
				}
			}
		}
	}
	return nodes
}

// getServiceAddresses get the IPs of services in the metallb configmap
func getServiceAddresses(config *metallb.ConfigFile) []string {
	ips := []string{}
	pools := config.Pools
	for _, p := range pools {
		ips = append(ips, p.Addresses...)
	}
	return ips
}

// addNodePeer update the configmap to ensure that the given node has the given peer
func addNodePeer(config *metallb.ConfigFile, nodeName string, localASN, peerASN int, peers ...string) (*metallb.ConfigFile, bool) {
	ns := metallb.NodeSelector{
		MatchLabels: map[string]string{
			hostnameKey: nodeName,
		},
	}
	var changed bool
	for _, peer := range peers {
		p := metallb.Peer{
			MyASN:         uint32(localASN),
			ASN:           uint32(peerASN),
			Addr:          peer,
			NodeSelectors: []metallb.NodeSelector{ns},
		}
		if config.AddPeer(&p) {
			changed = true
		}
	}
	return config, changed
}

// removeNodePeer update the configmap to ensure that the given node does not have the peer
func removeNodePeer(config *metallb.ConfigFile, nodeName string) (*metallb.ConfigFile, bool) {
	// go through the peers and see if we have one with our hostname.
	selector := metallb.NodeSelector{
		MatchLabels: map[string]string{
			hostnameKey: nodeName,
		},
	}
	var changed bool
	if config.RemovePeerBySelector(&selector) {
		changed = true
	}
	return config, changed
}

func getMetalConfigMap(getter typedv1.ConfigMapInterface, name string) (*metallb.ConfigFile, error) {
	cm, err := getter.Get(name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to get metallb configmap %s: %v", name, err)
	}
	var (
		configData string
		ok         bool
	)
	if configData, ok = cm.Data["config"]; !ok {
		return nil, errors.New("configmap data has no property 'config'")
	}
	return metallb.ParseConfig([]byte(configData))
}

func saveUpdatedConfigMap(cmi typedv1.ConfigMapInterface, name string, cfg *metallb.ConfigFile) error {
	b, err := cfg.Bytes()
	if err != nil {
		return fmt.Errorf("error converting configfile data to bytes: %v", err)
	}

	mergePatch, _ := json.Marshal(map[string]interface{}{
		"data": map[string]interface{}{
			"config": string(b),
		},
	})

	klog.V(2).Infof("patching configmap:\n%s", mergePatch)
	// save to k8s
	_, err = cmi.Patch(name, k8stypes.MergePatchType, mergePatch)

	return err
}

func serviceRep(svc *v1.Service) string {
	if svc == nil {
		return ""
	}
	return fmt.Sprintf("%s/%s", svc.Namespace, svc.Name)
}

func serviceTag(svc *v1.Service) string {
	if svc == nil {
		return ""
	}
	hash := sha256.Sum256([]byte(serviceRep(svc)))
	return fmt.Sprintf("service=%s", base64.StdEncoding.EncodeToString(hash[:]))
}

// unmapIP remove a given IP address from the metalllb config map
func unmapIP(config *metallb.ConfigFile, addr, configmapnamespace, configmapname string, k8sclient kubernetes.Interface) error {
	klog.V(2).Infof("unmapping IP %s", addr)
	return updateMapIP(config, addr, "", configmapnamespace, configmapname, k8sclient, false)
}

// mapIP add a given ip address to the metallb configmap
func mapIP(config *metallb.ConfigFile, addr, svcName, configmapnamespace, configmapname string, k8sclient kubernetes.Interface) error {
	klog.V(2).Infof("mapping IP %s", addr)
	return updateMapIP(config, addr, svcName, configmapnamespace, configmapname, k8sclient, true)
}
func updateMapIP(config *metallb.ConfigFile, addr, svcName, configmapnamespace, configmapname string, k8sclient kubernetes.Interface, add bool) error {
	if config == nil {
		klog.V(2).Info("config unchanged, not updating")
		return nil
	}
	// update the configmap and save it
	if add {
		autoAssign := false
		if !config.AddAddressPool(&metallb.AddressPool{
			Protocol:   "bgp",
			Name:       svcName,
			Addresses:  []string{addr},
			AutoAssign: &autoAssign,
		}) {
			klog.V(2).Info("address already on ConfigMap, unchanged")
			return nil
		}
	} else {
		config.RemoveAddressPoolByAddress(addr)
	}
	klog.V(2).Info("config changed, updating")
	if err := saveUpdatedConfigMap(k8sclient.CoreV1().ConfigMaps(configmapnamespace), configmapname, config); err != nil {
		klog.V(2).Infof("error updating configmap: %v", err)
		return fmt.Errorf("failed to update configmap: %v", err)
	}
	return nil
}
