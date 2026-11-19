package main

import (
	"context"
	"errors"

	"github.com/Mikimoto/cuirass"
)

const systemPrompt = "你是小型 coding agent。先 list_files 看有哪些檔,再 read_file 讀,需要時用 write_file 修改,最後簡短回報。"

// closed 建一個「封閉」schema:additionalProperties:false 是 cuirass 註冊工具的硬性要求,
// 模型多塞的參數會被 L2 的 schema 驗證直接擋下。
func closed(props map[string]any, required ...string) cuirass.Schema {
	if required == nil {
		required = []string{}
	}
	return cuirass.Schema{"type": "object", "additionalProperties": false, "properties": props, "required": required}
}

func str(desc string, maxLen int) map[string]any {
	return map[string]any{"type": "string", "description": desc, "maxLength": maxLen}
}

// Tools 回傳操作 ws 的四個工具。
// 全部宣告 EffectsNone(因為只碰記憶體);AuditArgs 只列 path / query,
// 檔案內容 content 不進稽核。
func Tools(ws *Workspace) []cuirass.ToolDef {
	fail := func(err error) (cuirass.ToolOutput, error) {
		// 工具錯誤用 IsError 回給模型看,讓它自己修正;不要回 Go error(那代表工具本身壞了)。
		return cuirass.ToolOutput{Content: err.Error(), IsError: true}, nil
	}
	return []cuirass.ToolDef{
		{
			Name: "list_files", Description: "列出工作區所有檔案", Effects: cuirass.EffectsNone,
			Parameters: closed(map[string]any{}),
			Execute: func(context.Context, map[string]any, cuirass.ToolCtx) (cuirass.ToolOutput, error) {
				return cuirass.ToolOutput{Content: ws.List()}, nil
			},
		},
		{
			Name: "read_file", Description: "讀取一個檔案", Effects: cuirass.EffectsNone,
			Parameters: closed(map[string]any{"path": str("相對路徑", maxPathLen)}, "path"),
			AuditArgs:  []string{"path"},
			Execute: func(_ context.Context, a map[string]any, _ cuirass.ToolCtx) (cuirass.ToolOutput, error) {
				c, err := ws.Read(a["path"].(string))
				if err != nil {
					return fail(err)
				}
				return cuirass.ToolOutput{Content: c}, nil
			},
		},
		{
			Name: "write_file", Description: "寫入(新增或覆蓋)一個檔案", Effects: cuirass.EffectsNone,
			Parameters: closed(map[string]any{"path": str("相對路徑", maxPathLen), "content": str("完整檔案內容", maxFileSize)}, "path", "content"),
			AuditArgs:  []string{"path"},
			Execute: func(_ context.Context, a map[string]any, _ cuirass.ToolCtx) (cuirass.ToolOutput, error) {
				if err := ws.Write(a["path"].(string), a["content"].(string)); err != nil {
					return fail(err)
				}
				return cuirass.ToolOutput{Content: "ok"}, nil
			},
		},
		{
			Name: "search", Description: "在所有檔案逐行搜尋子字串", Effects: cuirass.EffectsNone,
			Parameters: closed(map[string]any{"query": str("要找的文字", 200)}, "query"),
			AuditArgs:  []string{"query"},
			Execute: func(_ context.Context, a map[string]any, _ cuirass.ToolCtx) (cuirass.ToolOutput, error) {
				q := a["query"].(string)
				if q == "" {
					return fail(errors.New("query 不可為空"))
				}
				return cuirass.ToolOutput{Content: ws.Search(q)}, nil
			},
		},
	}
}

// NewAgent 把 provider、工作區、稽核組成一個 agent。audit 可為 nil(不記錄)。
// Policy.Tools 是白名單:只有列名的工具模型才叫得動。
func NewAgent(p *cuirass.Provider, ws *Workspace, audit cuirass.AuditSink) (*cuirass.Agent, error) {
	return cuirass.New(cuirass.AgentOptions{
		Providers: []*cuirass.Provider{p},
		System:    func() string { return systemPrompt },
		Tools:     Tools(ws),
		Policy:    cuirass.PolicyOptions{Tools: []string{"list_files", "read_file", "write_file", "search"}},
		Audit:     audit,
	})
}
