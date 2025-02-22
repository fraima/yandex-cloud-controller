package yandex

import (
	"context"
	"fmt"
	"sync"

	"github.com/pkg/errors"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/operation"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/vpc/v1"
	"google.golang.org/genproto/protobuf/field_mask"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
)

const (
	cpiRouteLabelsPrefix = "yandex.cpi.flant.com/"
	cpiNodeRoleLabel     = cpiRouteLabelsPrefix + "node-role" // we store Node's name here. The reason for this is lost in time (like tears in rain).
)

// these may get called in parallel, but since we have to modify the whole Route Table, we'll synchronize operations
var routeAPILock sync.Mutex

func (yc *Cloud) ListRoutes(ctx context.Context, _ string) ([]*cloudprovider.Route, error) {
	klog.Info("ListRoutes called")

	if routeAPILock.TryLock() {
		defer routeAPILock.Unlock()
	} else {
		return nil, errors.New("VPC route API locked")
	}

	req := &vpc.GetRouteTableRequest{
		RouteTableId: yc.config.RouteTableID,
	}

	routeTable, err := yc.yandexService.VPCSvc.RouteTableSvc.Get(ctx, req)
	if err != nil {
		return nil, err
	}

	var cpiRoutes []*cloudprovider.Route
	for _, staticRoute := range routeTable.StaticRoutes {
		var (
			nodeName string
			ok       bool
		)

		if nodeName, ok = staticRoute.Labels[cpiNodeRoleLabel]; !ok {
			continue
		}

		cpiRoutes = append(cpiRoutes, &cloudprovider.Route{
			Name:            nodeName,
			TargetNode:      types.NodeName(nodeName),
			DestinationCIDR: staticRoute.Destination.(*vpc.StaticRoute_DestinationPrefix).DestinationPrefix,
		})
	}

	return cpiRoutes, nil
}

func (yc *Cloud) CreateRoute(ctx context.Context, _ string, _ string, route *cloudprovider.Route) error {
	klog.Infof("CreateRoute called with %+v", *route)

	if routeAPILock.TryLock() {
		defer routeAPILock.Unlock()
	} else {
		return errors.New("VPC route API locked")
	}

	rt, err := yc.yandexService.VPCSvc.RouteTableSvc.Get(ctx, &vpc.GetRouteTableRequest{RouteTableId: yc.config.RouteTableID})
	if err != nil {
		return err
	}

	kubeNodeName := string(route.TargetNode)
	nextHop, err := yc.getInternalIpByNodeName(kubeNodeName)
	if err != nil {
		return err
	}

	newStaticRoutes := filterStaticRoutes(rt.StaticRoutes, routeFilterTerm{
		termType:        routeFilterAddOrUpdate,
		nodeName:        kubeNodeName,
		destinationCIDR: route.DestinationCIDR,
		nextHop:         nextHop,
	})

	req := &vpc.UpdateRouteTableRequest{
		RouteTableId: yc.config.RouteTableID,
		UpdateMask: &field_mask.FieldMask{
			Paths: []string{"static_routes"},
		},
		StaticRoutes: newStaticRoutes,
	}

	_, _, err = yc.yandexService.OperationWaiter(ctx, func() (*operation.Operation, error) { return yc.yandexService.VPCSvc.RouteTableSvc.Update(ctx, req) })
	return err
}

func (yc *Cloud) DeleteRoute(ctx context.Context, _ string, route *cloudprovider.Route) error {
	klog.Infof("DeleteRoute called with %+v", *route)

	if routeAPILock.TryLock() {
		defer routeAPILock.Unlock()
	} else {
		return errors.New("VPC route API locked")
	}

	rt, err := yc.yandexService.VPCSvc.RouteTableSvc.Get(ctx, &vpc.GetRouteTableRequest{RouteTableId: yc.config.RouteTableID})
	if err != nil {
		return err
	}

	nodeNameToDelete := string(route.TargetNode)
	newStaticRoutes := filterStaticRoutes(rt.StaticRoutes, routeFilterTerm{
		termType: routeFilterRemove,
		nodeName: nodeNameToDelete,
	})

	req := &vpc.UpdateRouteTableRequest{
		RouteTableId: yc.config.RouteTableID,
		UpdateMask: &field_mask.FieldMask{
			Paths: []string{"static_routes"},
		},
		StaticRoutes: newStaticRoutes,
	}

	_, _, err = yc.yandexService.OperationWaiter(ctx, func() (*operation.Operation, error) { return yc.yandexService.VPCSvc.RouteTableSvc.Update(ctx, req) })
	return err
}

func (yc *Cloud) getInternalIpByNodeName(nodeName string) (string, error) {
	kubeNode, err := yc.nodeLister.Get(nodeName)
	if err != nil {
		return "", err
	}

	var targetInternalIP string
	for _, address := range kubeNode.Status.Addresses {
		if address.Type == v1.NodeInternalIP {
			targetInternalIP = address.Address
		}
	}
	if len(targetInternalIP) == 0 {
		return "", fmt.Errorf("no InternalIPs found for Node %q", nodeName)
	}

	return targetInternalIP, nil
}

type routeFilterTerm struct {
	termType        routeFilterTermType
	nodeName        string
	destinationCIDR string
	nextHop         string
}

type routeFilterTermType string

const (
	routeFilterAddOrUpdate routeFilterTermType = "AddOrUpdate"
	routeFilterRemove      routeFilterTermType = "Remove"
)

func filterStaticRoutes(staticRoutes []*vpc.StaticRoute, filterTerms ...routeFilterTerm) (ret []*vpc.StaticRoute) {
	var nodeNamesUpdatedSet = make(map[string]struct{})

	for _, existingStaticRoute := range staticRoutes {
		var (
			nodeName string
			ok       bool
		)

		if nodeName, ok = existingStaticRoute.Labels[cpiNodeRoleLabel]; !ok {
			ret = append(ret, existingStaticRoute)
			continue
		}

		var deleteRoute bool
		var routeAppended bool
		for _, filter := range filterTerms {
			if nodeName != filter.nodeName {
				continue
			}

			if filter.termType == routeFilterAddOrUpdate {
				ret = append(ret, &vpc.StaticRoute{
					Destination: &vpc.StaticRoute_DestinationPrefix{DestinationPrefix: filter.destinationCIDR},
					NextHop:     &vpc.StaticRoute_NextHopAddress{NextHopAddress: filter.nextHop},
					Labels:      existingStaticRoute.Labels,
				})

				nodeNamesUpdatedSet[nodeName] = struct{}{}
				routeAppended = true
				break
			}

			if filter.termType == routeFilterRemove {
				klog.Infof("Removing %+v StaticRoute from Yandex.Cloud", existingStaticRoute)
				deleteRoute = true
				break
			}
		}

		if !deleteRoute && !routeAppended {
			ret = append(ret, existingStaticRoute)
		}
	}

	// final iteration to add missing routes
	for _, filter := range filterTerms {
		if filter.termType == routeFilterAddOrUpdate {
			if _, updated := nodeNamesUpdatedSet[filter.nodeName]; !updated {
				ret = append(ret, &vpc.StaticRoute{
					Destination: &vpc.StaticRoute_DestinationPrefix{DestinationPrefix: filter.destinationCIDR},
					NextHop:     &vpc.StaticRoute_NextHopAddress{NextHopAddress: filter.nextHop},
					Labels:      map[string]string{cpiNodeRoleLabel: filter.nodeName},
				})
			}
		}
	}

	return
}
