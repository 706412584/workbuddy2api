// sse.go 上游 SSE 流的 chunk 读取。
//
// 上游（upstream.Stream 的写入侧）恒以 `data: {json}\n\n` 帧输出，以 `data: [DONE]` 收尾。
// 协议适配层需要把每个 chunk 交给 apicompat 的状态机转成客户端协议的事件，故在此逐帧解析。
package server

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"workbuddy2api/internal/apicompat"
)

// chatChunkReader 把上游 chat completions 的 SSE 流解析为 chunk 序列。
type chatChunkReader struct {
	br *bufio.Reader
}

func newChatChunkReader(r io.Reader) *chatChunkReader {
	return &chatChunkReader{br: bufio.NewReaderSize(r, 64*1024)}
}

// next 返回下一个 chunk。流结束（收到 [DONE] 或读到 EOF）返回 (nil, io.EOF)。
//
// 无法解析的帧被跳过而不中断流：上游可能插入注释行、心跳或非 JSON 帧，
// 为此让整轮对话失败是过度反应。但读错误（非 EOF）会如实返回，避免把断流伪装成正常结束。
func (c *chatChunkReader) next() (*apicompat.ChatCompletionsChunk, error) {
	for {
		line, err := c.br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")

		if payload, ok := strings.CutPrefix(trimmed, "data: "); ok {
			if payload == "[DONE]" {
				return nil, io.EOF
			}
			var ch apicompat.ChatCompletionsChunk
			if json.Unmarshal([]byte(payload), &ch) == nil {
				return &ch, nil
			}
			// 解析失败：继续读下一帧（err 在下方统一处理）
		}

		if err != nil {
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, err
		}
	}
}
