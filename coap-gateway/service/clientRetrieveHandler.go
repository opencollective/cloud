package service

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/go-ocf/cloud/coap-gateway/coapconv"
	pbRA "github.com/go-ocf/cloud/resource-aggregate/pb"
	pbRS "github.com/go-ocf/cloud/resource-directory/pb/resource-shadow"
	gocoap "github.com/go-ocf/go-coap"
	coapCodes "github.com/go-ocf/go-coap/codes"
	"github.com/go-ocf/kit/log"
	kitNetGrpc "github.com/go-ocf/kit/net/grpc"
	"github.com/gofrs/uuid"
	"google.golang.org/grpc/status"
)

// URIToDeviceIDHref convert uri to deviceID and href. Expected input "/oic/route/{deviceID}/{Href}".
func URIToDeviceIDHref(msg gocoap.Message) (deviceID, href string, err error) {
	path := msg.Path()
	if len(path) < 4 {
		return "", "", fmt.Errorf("cannot parse deviceID, href from uri")
	}
	return path[2], fixHref(strings.Join(path[3:], "/")), nil
}

func getResourceInterface(msg gocoap.Message) string {
	for _, queryRaw := range msg.Options(gocoap.URIQuery) {
		if query, ok := queryRaw.(string); ok && strings.HasPrefix(query, "if=") {
			return strings.TrimLeft(query, "if=")
		}
	}
	return ""
}

func clientRetrieveHandler(s gocoap.ResponseWriter, req *gocoap.Request, client *Client) {
	t := time.Now()
	defer func() {
		log.Debugf("clientRetrieveHandler takes %v", time.Since(t))
	}()
	authCtx := client.loadAuthorizationContext()

	deviceID, href, err := URIToDeviceIDHref(req.Msg)
	if err != nil {
		logAndWriteErrorResponse(fmt.Errorf("DeviceId: %v: cannot handle retrieve resource: %w", authCtx.DeviceId, err), s, client, coapCodes.BadRequest)
		return
	}

	var content *pbRA.Content
	var code coapCodes.Code
	resourceInterface := getResourceInterface(req.Msg)
	resourceID := resource2UUID(deviceID, href)
	if resourceInterface == "" {
		content, code, err = clientRetrieveFromResourceShadowHandler(kitNetGrpc.CtxWithToken(req.Ctx, authCtx.AccessToken), client, resourceID)
		if err != nil {
			logAndWriteErrorResponse(fmt.Errorf("DeviceId: %v: cannot retrieve resource /%v%v from resource shadow: %w", authCtx.DeviceId, deviceID, href, err), s, client, code)
			return
		}
	} else {
		content, code, err = clientRetrieveFromDeviceHandler(req, client, deviceID, resourceID, resourceInterface)
		if err != nil {
			logAndWriteErrorResponse(fmt.Errorf("DeviceId: %v: cannot retrieve resource /%v%v from device: %w", authCtx.DeviceId, deviceID, href, err), s, client, code)
			return
		}
	}

	if content == nil || len(content.Data) == 0 {
		sendResponse(s, client, code, gocoap.TextPlain, nil)
		return
	}
	mediaType, err := coapconv.MakeMediaType(content.CoapContentFormat, content.ContentType)
	if err != nil {
		logAndWriteErrorResponse(fmt.Errorf("DeviceId: %v: cannot retrieve resource /%v%v: %w", authCtx.DeviceId, deviceID, href, err), s, client, code)
		return
	}
	sendResponse(s, client, code, mediaType, content.Data)
}

func clientRetrieveFromResourceShadowHandler(ctx context.Context, client *Client, resourceID string) (*pbRA.Content, coapCodes.Code, error) {
	authCtx := client.loadAuthorizationContext()
	retrieveResourcesValuesClient, err := client.server.rsClient.RetrieveResourcesValues(kitNetGrpc.CtxWithToken(ctx, authCtx.AccessToken), &pbRS.RetrieveResourcesValuesRequest{
		ResourceIdsFilter:    []string{resourceID},
		AuthorizationContext: &authCtx.AuthorizationContext,
	})
	if err != nil {
		return nil, coapconv.GrpcCode2CoapCode(status.Convert(err).Code(), coapCodes.GET), err
	}
	defer retrieveResourcesValuesClient.CloseSend()
	for {
		resourceValue, err := retrieveResourcesValuesClient.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, coapconv.GrpcCode2CoapCode(status.Convert(err).Code(), coapCodes.GET), err
		}
		if resourceValue.ResourceId == resourceID && resourceValue.Content != nil {
			return resourceValue.Content, coapCodes.Content, nil
		}
	}
	return nil, coapCodes.NotFound, fmt.Errorf("not found")
}

func clientRetrieveFromDeviceHandler(req *gocoap.Request, client *Client, deviceID, resourceID, resourceInterface string) (*pbRA.Content, coapCodes.Code, error) {
	authCtx := client.loadAuthorizationContext()
	correlationIDUUID, err := uuid.NewV4()
	if err != nil {
		return nil, coapCodes.InternalServerError, err
	}

	correlationID := correlationIDUUID.String()

	notify := client.server.retrieveNotificationContainer.Add(correlationID)
	defer client.server.retrieveNotificationContainer.Remove(correlationID)

	loaded, err := client.server.projection.Register(req.Ctx, deviceID)
	if err != nil {
		return nil, coapCodes.NotFound, fmt.Errorf("cannot register device to projection: %w", err)
	}
	defer client.server.projection.Unregister(deviceID)
	if !loaded {
		if len(client.server.projection.Models(deviceID, resourceID)) == 0 {
			err = client.server.projection.ForceUpdate(req.Ctx, deviceID, resourceID)
			if err != nil {
				return nil, coapCodes.NotFound, err
			}
		}
	}

	request := coapconv.MakeRetrieveResourceRequest(resourceID, resourceInterface, correlationID, authCtx.AuthorizationContext, req)

	_, err = client.server.raClient.RetrieveResource(kitNetGrpc.CtxWithToken(req.Ctx, authCtx.AccessToken), &request)
	if err != nil {
		return nil, coapconv.GrpcCode2CoapCode(status.Convert(err).Code(), coapCodes.GET), err
	}

	// first wait for notification
	timeoutCtx, cancel := context.WithTimeout(req.Ctx, client.server.RequestTimeout)
	defer cancel()
	select {
	case processed := <-notify:
		return processed.GetContent(), coapconv.StatusToCoapCode(processed.Status, coapCodes.GET), nil
	case <-timeoutCtx.Done():
		return nil, coapCodes.GatewayTimeout, fmt.Errorf("timeout")
	}
}
