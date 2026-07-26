package bridge

import (
	"strings"
	"unicode/utf8"
)

// 私聊任务判定（ADR-0006）：用户在私聊里发出「明确的任务」时，
// 桥接器自动建群并在群里启动任务；闲聊/短问句则留在私聊里直接回答。
// 这里是纯启发式规则，确定性、可测试，规则可后续调优。

// taskKeywords 命中任意一个即视为任务意图。
var taskKeywords = []string{
	// 中文动作词
	"帮我", "麻烦", "请帮", "帮忙",
	"实现", "修复", "写一个", "写个", "写一篇", "写一下",
	"创建", "新建", "部署", "分析", "总结", "翻译",
	"解释", "排查", "调试", "优化", "重构", "审查",
	"查一下", "查看", "看一下", "看看", "检查",
	"改成", "修改", "加上", "新增", "删除", "去掉",
	"跑一下", "运行", "执行", "整理", "对比", "比较", "设计",
	// 英文动作词
	"fix", "bug", "implement", "create", "write", "refactor",
	"debug", "deploy", "review", "summarize", "translate",
	"analyze", "analyse", "optimize", "add", "remove",
	"update", "explain", "build",
}

// smalltalk 是完全匹配才算的闲聊短句（strip 标点/空白后比较）。
var smalltalk = map[string]bool{
	"hi": true, "hello": true, "hey": true, "yo": true,
	"ok": true, "okay": true, "yes": true, "no": true,
	"thanks": true, "thankyou": true, "thx": true,
	"你好": true, "您好": true, "在吗": true, "在么": true,
	"谢谢": true, "感谢": true, "好的": true, "好": true,
	"嗯": true, "嗯嗯": true, "收到": true, "了解": true,
	"哈哈": true, "早": true, "早上好": true, "晚安": true,
	"测试": true, "test": true,
}

// longMessageRunes 超过这个长度的私聊消息直接视为任务（长消息通常
// 携带具体诉求或上下文）。
const longMessageRunes = 20

// LooksLikeTask 报告一条私聊消息是否像「明确的任务」。
// hasResources 为 true（带图片/文件）时直接判定为任务。
func LooksLikeTask(content string, hasResources bool) bool {
	if hasResources {
		return true
	}
	text := strings.TrimSpace(content)
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)

	// 完全匹配的闲聊短句优先（即使含动作词，如「谢谢」不该建群）。
	squashed := strings.NewReplacer(" ", "", "!", "", "！", "",
		"?", "", "？", "", "。", "", "~", "", "～", "", ",", "", "，", "").Replace(lower)
	if smalltalk[squashed] {
		return false
	}
	for _, kw := range taskKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return utf8.RuneCountInString(text) >= longMessageRunes
}
