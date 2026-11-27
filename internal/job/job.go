// Package job owns the recording job: its status machine and the Postgres queue that hands it to workers.
package job

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Ishoshot/debrief/internal/ai"
)

type (
	// Status moves only through the Store methods in queue.go:
	//
	//	queued -> processing                  Claim
	//	processing -> processing              Claim (lease expired and attempts left)
	//	processing -> succeeded               Succeed
	//	processing -> queued | failed         Retry (failed once attempts are used up)
	//	processing -> failed                  Fail, or FailExpired when the final attempt's lease expired
	//	processing -> queued                  Release (shutdown; the job gets one more attempt)
	Status string

	Job struct {
		ID          uuid.UUID
		Status      Status
		Filename    string
		ContentType string
		Attempts    int
		MaxAttempts int
		LastError   string
		Result      *ai.Result
		Delivery    *Delivery
		CreatedAt   time.Time
		UpdatedAt   time.Time
	}

	// ResponseBody can echo customer data, so never log it or return it from the API.
	Failure struct {
		Message      string
		ResponseBody string
	}

	Delivery struct {
		Destination string    `json:"destination"`
		Reference   string    `json:"reference,omitempty"`
		DeliveredAt time.Time `json:"delivered_at"`
	}

	New struct {
		IdempotencyKey string
		Filename       string
		ContentType    string
		Audio          []byte
	}

	Claimed struct {
		Job
		Audio []byte
	}

	// The attempt number fences writes: after another worker claims the job, the old holder's writes match no row.
	Lease struct {
		ID      uuid.UUID
		Attempt int
	}
)

const (
	Queued     Status = "queued"
	Processing Status = "processing"
	Succeeded  Status = "succeeded"
	Failed     Status = "failed"
)

var (
	ErrNotFound  = errors.New("job not found")
	ErrNoJob     = errors.New("no job ready")
	ErrLeaseLost = errors.New("job lease lost")
	ErrKeyReused = errors.New("idempotency key reused with different audio")
)

func (c Claimed) Lease() Lease { return Lease{ID: c.ID, Attempt: c.Attempts} }
