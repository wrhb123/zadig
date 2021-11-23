package task

import (
	"fmt"

	"github.com/koderover/zadig/pkg/microservice/aslan/config"
)

type Trigger struct {
	TaskType        config.TaskType  `bson:"type"                       json:"type"`
	Enabled         bool             `bson:"enabled"                    json:"enabled"`
	TaskStatus      config.Status    `bson:"status"                     json:"status"`
	URL             string           `bson:"url,omitempty"              json:"url,omitempty"`
	Path            string           `bson:"path,omitempty"             json:"path,omitempty"`
	IsCallback      bool             `bson:"is_callback,omitempty"      json:"is_callback,omitempty"`
	CallbackType    string           `bson:"callback_type,omitempty"    json:"callback_type,omitempty"`
	CallbackPayload *CallbackPayload `bson:"callback_payload,omitempty" json:"callback_payload,omitempty"`
	Timeout         int              `bson:"timeout"                    json:"timeout,omitempty"`
	IsRestart       bool             `bson:"is_restart"                 json:"is_restart"`
	Error           string           `bson:"error,omitempty"            json:"error,omitempty"`
	StartTime       int64            `bson:"start_time"                 json:"start_time,omitempty"`
	EndTime         int64            `bson:"end_time"                   json:"end_time,omitempty"`
}

type CallbackPayload struct {
	QRCodeURL string `bson:"QR_code_URL,omitempty" json:"QR_code_URL,omitempty"`
}

// ToSubTask ...
func (t *Trigger) ToSubTask() (map[string]interface{}, error) {
	var task map[string]interface{}
	if err := IToi(t, &task); err != nil {
		return nil, fmt.Errorf("convert trigger to interface error: %s", err)
	}
	return task, nil
}