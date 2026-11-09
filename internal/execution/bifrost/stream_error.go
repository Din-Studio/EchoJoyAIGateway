package bifrost

import (
	"encoding/json"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"gpt-load/internal/protocol"
)

type streamErrorStatusKey struct{}

func captureStreamErrorStatus(ctx *schemas.BifrostContext, upstream protocol.Protocol) {
	if upstream == protocol.OpenAICompletions || upstream == protocol.OpenAIResponses {
		// 0 表示没有错误事件，-1 表示事件未携带状态，正数是上游显式状态。
		ctx.SetValue(streamErrorStatusKey{}, 0)
	}
}

type streamErrorDataReader struct {
	providerUtils.SSEDataReader
	ctx *schemas.BifrostContext
}

func (reader *streamErrorDataReader) ReadDataLine() ([]byte, error) {
	reader.ctx.SetValue(streamErrorStatusKey{}, 0)
	data, err := reader.SSEDataReader.ReadDataLine()
	if err != nil {
		return data, err
	}
	eventType := gjson.GetBytes(data, "type").String()
	if eventType != "error" && eventType != "response.failed" && !gjson.GetBytes(data, "error").IsObject() {
		return data, nil
	}
	var event struct {
		StatusCode *int `json:"status_code"`
	}
	if json.Unmarshal(data, &event) == nil {
		status := -1
		if event.StatusCode != nil && *event.StatusCode >= 100 && *event.StatusCode <= 599 {
			status = *event.StatusCode
		}
		reader.ctx.SetValue(streamErrorStatusKey{}, status)
	}
	return data, nil
}

// 保留 SDK 对 [DONE]、截断及 finish_reason 后心跳的判断。
func (reader *streamErrorDataReader) SawDoneMarker() bool {
	return providerUtils.SSEStreamEndedOnMarker(reader.SSEDataReader)
}

func (reader *streamErrorDataReader) EndOnCommentAfterFinish() {
	providerUtils.SSEEndOnCommentAfterFinish(reader.SSEDataReader)
}

func (reader *streamErrorDataReader) EndedOnComment() bool {
	return providerUtils.SSEEndedOnComment(reader.SSEDataReader)
}

// 首帧提升错误和后续错误共用此出口；真实 HTTP 拒绝没有错误事件记录。
func restoreStreamErrorStatus(ctx *schemas.BifrostContext, err *schemas.BifrostError) *schemas.BifrostError {
	if ctx == nil || err == nil {
		return err
	}
	status, _ := ctx.GetAndSetValue(streamErrorStatusKey{}, 0).(int)
	if status == 0 {
		return err
	}
	copyError := *err
	copyError.StatusCode = nil
	if status > 0 {
		copyError.StatusCode = schemas.Ptr(status)
	}
	return &copyError
}
