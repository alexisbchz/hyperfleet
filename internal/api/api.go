package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/alexis-bouchez/hyperfleet/internal/vmmgr"
	"github.com/danielgtaylor/huma/v2"
)

type handler struct {
	mgr *vmmgr.Manager
}

func Register(api huma.API, mgr *vmmgr.Manager) {
	h := &handler{mgr: mgr}

	huma.Register(api, huma.Operation{
		OperationID:   "create-machine",
		Method:        http.MethodPost,
		Path:          "/machines",
		Summary:       "Create a machine",
		Description:   "Provisions a new microVM asynchronously. Returns immediately with status=pending.",
		Tags:          []string{"machines"},
		DefaultStatus: http.StatusAccepted,
	}, h.create)

	huma.Register(api, huma.Operation{
		OperationID: "list-machines",
		Method:      http.MethodGet,
		Path:        "/machines",
		Summary:     "List machines",
		Tags:        []string{"machines"},
	}, h.list)

	huma.Register(api, huma.Operation{
		OperationID: "get-machine",
		Method:      http.MethodGet,
		Path:        "/machines/{id}",
		Summary:     "Get a machine",
		Tags:        []string{"machines"},
	}, h.get)

	huma.Register(api, huma.Operation{
		OperationID:   "delete-machine",
		Method:        http.MethodDelete,
		Path:          "/machines/{id}",
		Summary:       "Delete a machine",
		Description:   "Stops the microVM, releases its snapshot, and removes the record.",
		Tags:          []string{"machines"},
		DefaultStatus: http.StatusNoContent,
	}, h.delete)
}

func (h *handler) create(ctx context.Context, in *CreateMachineInput) (*CreateMachineOutput, error) {
	image := strings.TrimSpace(in.Body.Image)
	if image == "" {
		return nil, huma.Error400BadRequest("image is required")
	}
	m, err := h.mgr.Create(ctx, image)
	if err != nil {
		return nil, err
	}
	return &CreateMachineOutput{Body: toDTO(m)}, nil
}

func (h *handler) list(ctx context.Context, _ *struct{}) (*ListMachinesOutput, error) {
	machines := h.mgr.List(ctx)
	out := &ListMachinesOutput{}
	out.Body.Machines = make([]MachineDTO, 0, len(machines))
	for _, m := range machines {
		out.Body.Machines = append(out.Body.Machines, toDTO(m))
	}
	return out, nil
}

func (h *handler) get(ctx context.Context, in *MachineIDInput) (*GetMachineOutput, error) {
	m, err := h.mgr.Get(ctx, in.ID)
	if err != nil {
		if errors.Is(err, vmmgr.ErrNotFound) {
			return nil, huma.Error404NotFound("machine not found")
		}
		return nil, err
	}
	return &GetMachineOutput{Body: toDTO(m)}, nil
}

func (h *handler) delete(ctx context.Context, in *MachineIDInput) (*struct{}, error) {
	if err := h.mgr.Delete(ctx, in.ID); err != nil {
		if errors.Is(err, vmmgr.ErrNotFound) {
			return nil, huma.Error404NotFound("machine not found")
		}
		return nil, err
	}
	return nil, nil
}
