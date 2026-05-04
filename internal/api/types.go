package api

import (
	"time"

	"github.com/alexis-bouchez/hyperfleet/internal/vmmgr"
)

type MachineDTO struct {
	ID        string     `json:"id" example:"ck5g9k1xa0000g0qja1xrjgqa" doc:"CUID identifier"`
	Image     string     `json:"image" example:"docker.io/library/alpine:3.20" doc:"OCI image reference"`
	Status    string     `json:"status" enum:"pending,running,exited,failed"`
	CreatedAt time.Time  `json:"createdAt"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	ExitedAt  *time.Time `json:"exitedAt,omitempty"`
	Error     string     `json:"error,omitempty"`
}

func toDTO(m vmmgr.Machine) MachineDTO {
	return MachineDTO{
		ID:        m.ID,
		Image:     m.Image,
		Status:    string(m.Status),
		CreatedAt: m.CreatedAt,
		StartedAt: m.StartedAt,
		ExitedAt:  m.ExitedAt,
		Error:     m.Error,
	}
}

type CreateMachineInput struct {
	Body struct {
		Image string `json:"image" example:"docker.io/library/alpine:3.20" doc:"OCI image reference" required:"true"`
	}
}

type CreateMachineOutput struct {
	Body MachineDTO
}

type ListMachinesOutput struct {
	Body struct {
		Machines []MachineDTO `json:"machines"`
	}
}

type MachineIDInput struct {
	ID string `path:"id" doc:"Machine CUID"`
}

type GetMachineOutput struct {
	Body MachineDTO
}
