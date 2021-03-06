package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	pbCQRS "github.com/go-ocf/cloud/resource-aggregate/pb"
	pbDD "github.com/go-ocf/cloud/resource-directory/pb/device-directory"
	kitNetGrpc "github.com/go-ocf/kit/net/grpc"
	kitNetHttp "github.com/go-ocf/kit/net/http"
	"github.com/go-ocf/sdk/schema"
	"github.com/gorilla/mux"
)

type Status string

const Status_ONLINE Status = "online"
const Status_OFFLINE Status = "offline"

func toStatus(isOnline bool) Status {
	if isOnline {
		return "online"
	}
	return "offline"
}

func toLocalizedString(s *pbDD.LocalizedString) schema.LocalizedString {
	return schema.LocalizedString{
		Value:    s.Value,
		Language: s.Language,
	}
}

func toLocalizedStrings(s []*pbDD.LocalizedString) []schema.LocalizedString {
	r := make([]schema.LocalizedString, 0, 16)
	for _, v := range s {
		r = append(r, toLocalizedString(v))
	}
	return r
}

type responseWriterEncoderFunc func(w http.ResponseWriter, v interface{}, status int) error

type Device struct {
	Device schema.Device `json:"device"`
	Status Status        `json:"status"`
}

func (rh *RequestHandler) GetDevices(ctx context.Context, deviceIdsFilter []string, authorizationContext pbCQRS.AuthorizationContext) ([]Device, error) {
	getDevicesClient, err := rh.ddClient.GetDevices(ctx, &pbDD.GetDevicesRequest{
		DeviceIdsFilter:      deviceIdsFilter,
		AuthorizationContext: &authorizationContext,
	})

	if err != nil {
		return nil, fmt.Errorf("cannot get devices: %w", err)
	}
	defer getDevicesClient.CloseSend()

	devices := make([]Device, 0, 32)
	for {
		device, err := getDevicesClient.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("cannot get devices: %w", err)
		}

		devices = append(devices, Device{
			Device: schema.Device{
				ID:               device.GetId(),
				ResourceTypes:    device.GetResource().GetResourceTypes(),
				Name:             device.GetResource().GetName(),
				ModelNumber:      device.GetResource().GetModelNumber(),
				ManufacturerName: toLocalizedStrings(device.GetResource().GetManufacturerName()),
			},
			Status: toStatus(device.IsOnline),
		})
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("cannot get devices: not found")
	}
	return devices, nil
}

func (rh *RequestHandler) RetrieveDevicesBase(ctx context.Context, w http.ResponseWriter, encoder responseWriterEncoderFunc) (int, error) {
	devices, err := rh.GetDevices(ctx, nil, pbCQRS.AuthorizationContext{})
	if err != nil {
		return kitNetHttp.ErrToStatusWithDef(err, http.StatusForbidden), fmt.Errorf("cannot retrieve all devices[base]: %w", err)
	}
	resourceLink, err := rh.GetResourceLinks(ctx, nil, pbCQRS.AuthorizationContext{})
	if err != nil {
		return kitNetHttp.ErrToStatusWithDef(err, http.StatusForbidden), fmt.Errorf("cannot retrieve all devices[base]: %w", err)
	}

	resp := make([]RetrieveDeviceWithLinksResponse, 0, 32)
	for _, dev := range devices {
		links, ok := resourceLink[dev.Device.ID]
		if ok {
			resp = append(resp, RetrieveDeviceWithLinksResponse{
				Device: dev,
				Links:  links,
			})
		}
	}

	err = encoder(w, resp, http.StatusOK)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("cannot retrieve all devices[base]: %w", err)
	}
	return http.StatusOK, nil
}

func (rh *RequestHandler) RetrieveDevicesAll(ctx context.Context, w http.ResponseWriter, encoder responseWriterEncoderFunc) (int, error) {
	devices, err := rh.GetDevices(ctx, nil, pbCQRS.AuthorizationContext{})
	if err != nil {
		return kitNetHttp.ErrToStatusWithDef(err, http.StatusForbidden), fmt.Errorf("cannot retrieve all devices[base]: %w", err)
	}
	reps, err := rh.RetrieveResourcesValues(ctx, nil, nil, pbCQRS.AuthorizationContext{})
	if err != nil {
		return kitNetHttp.ErrToStatusWithDef(err, http.StatusForbidden), fmt.Errorf("cannot retrieve all devices[base]: %w", err)
	}

	resp := make([]RetrieveDeviceContentAllResponse, 0, 32)
	for _, dev := range devices {
		devReps, ok := reps[dev.Device.ID]
		if ok {
			resp = append(resp, RetrieveDeviceContentAllResponse{
				Device: dev,
				Links:  devReps,
			})
		}
	}

	err = encoder(w, resp, http.StatusOK)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("cannot retrieve all devices[base]: %w", err)
	}
	return http.StatusOK, nil
}

func (rh *RequestHandler) RetrieveDevicesWithContentQuery(ctx context.Context, w http.ResponseWriter, routeVars map[string]string, contentQuery string, encoder responseWriterEncoderFunc) (statusCode int, err error) {
	switch contentQuery {
	case ContentQueryAllValue:
		statusCode, err = rh.RetrieveDevicesAll(ctx, w, encoder)
	case ContentQueryBaseValue:
		statusCode, err = rh.RetrieveDevicesBase(ctx, w, encoder)
	default:
		return http.StatusBadRequest, fmt.Errorf("invalid value '%v' of '%v' query parameter", contentQuery, ContentQuery)
	}
	if err != nil {
		statusCode = kitNetHttp.ErrToStatusWithDef(err, statusCode)
		if statusCode == http.StatusNotFound {
			// return's empty array
			errEnc := encoder(w, []interface{}{}, http.StatusOK)
			if errEnc == nil {
				return http.StatusOK, nil
			}
		}
	}
	return statusCode, err

}

type callbackFunc func(ctx context.Context, w http.ResponseWriter, routeVars map[string]string, contentQuery string, encoder responseWriterEncoderFunc) (int, error)

func getAccessToken(r *http.Request) (string, error) {
	token, _, err := parseAuth(r.Header.Get("Authorization"))
	if err != nil {
		return "", fmt.Errorf("cannot retrieve: %w", err)
	}

	return token, nil
}

func retrieveWithCallback(w http.ResponseWriter, r *http.Request, callback callbackFunc) (int, error) {
	accessToken, err := getAccessToken(r)
	if err != nil {
		return http.StatusUnauthorized, err
	}

	encoder, err := getResponseWriterEncoder(strings.Split(r.Header.Get("Accept"), ","))
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("cannot retrieve: %w", err)
	}

	return callback(kitNetGrpc.CtxWithToken(r.Context(), accessToken), w, mux.Vars(r), getContentQueryValue(r.URL), encoder)
}

func (rh *RequestHandler) RetrieveDevices(w http.ResponseWriter, r *http.Request) {
	statusCode, err := retrieveWithCallback(w, r, rh.RetrieveDevicesWithContentQuery)
	if err != nil {
		logAndWriteErrorResponse(fmt.Errorf("cannot retrieve all devices: %w", err), statusCode, w)
	}
}
