package main

import (
	"fmt"
	"time"
)

func preparePrimaryBody(raw []byte, protocol string, cfg runtimeConfig, callbackID string, event *bridgeEvent) ([]byte, int, error) {
	if len(raw) == 0 {
		return nil, 0, fmt.Errorf("original %s request is missing", protocol)
	}
	adapter, err := adapterForProtocol(protocol)
	if err != nil {
		return nil, 0, err
	}
	body, normalized, mayContainMedia, err := adapter.normalizeRequest(raw)
	if err != nil {
		return nil, 0, err
	}
	if normalized {
		cfg.events.stage(event, "规范化 Responses 输入", "完成", cfg.PrimaryModel, "字符串 input 已转换为等价的标准 user message 数组；其他请求参数保持不变。", time.Now())
	}
	processedImagesForTurn := 0
	body, images, err := transformRequestWithPlanAndMediaHint(body, adapter.protocol, cfg, mayContainMedia, func(asset visualAsset, contextText string) (string, error) {
		return describeImage(cfg, callbackID, asset, contextText, event)
	}, func(plan visualTransformPlan) {
		processedImagesForTurn = plan.CurrentImages + plan.RestoredImages
		if plan.HistoricalImages == 0 {
			return
		}
		detail := fmt.Sprintf("检测到 %d 张历史图片：%d 张替换为固定短归档标记，%d 张因本轮明确引用而恢复；当前轮图片 %d 张。", plan.HistoricalImages, plan.ArchivedImages, plan.RestoredImages, plan.CurrentImages)
		if plan.RestoredImages == 0 {
			detail += " 未解码旧图，也未调用视觉模型。"
		}
		cfg.events.stage(event, "历史图片处理", "完成", cfg.PrimaryModel, detail, time.Now())
	})
	if err != nil || images == 0 || processedImagesForTurn == 0 {
		return body, images, err
	}
	body, toolPolicy, err := applyProcessedImageToolPolicy(body, adapter)
	if err != nil {
		return nil, images, err
	}
	detail := "本轮相关图片已转换为视觉记忆，并加入不得仅为重复读取这些图片而调用客户端工具的约束。"
	if toolPolicy.RemovedViewImage {
		detail += " 已移除 view_image。"
	}
	if toolPolicy.ConstrainedTools > 0 {
		detail += fmt.Sprintf(" 已为 %d 个 shell_command/js 工具定义补充同一约束。", toolPolicy.ConstrainedTools)
	}
	detail += " 其他工具仍可用于用户明确要求的代码、文件、系统、外部资源或图片处理操作。"
	cfg.events.stage(event, "约束重复看图工具", "完成", cfg.PrimaryModel, detail, time.Now())
	return body, images, nil
}
func describeImage(cfg runtimeConfig, callbackID string, asset visualAsset, contextText string, event *bridgeEvent) (string, error) {
	// transformRequest validates every selected image before any visual call.
	key := visualCacheKey(cfg, asset, contextText)
	if key != "" {
		if cached, ok := cfg.cache.get(key); ok {
			cfg.events.stage(event, "读取视觉记忆缓存", "完成", "缓存", "同一图片命中本地内存缓存，未再次调用视觉模型。", time.Now())
			return cached, nil
		}
	}
	value, joined, err := cfg.cache.do(key, func() (string, error) {
		cfg.events.stage(event, "进入视觉候选链", "完成", "", fmt.Sprintf("仅携带当前用户附近文字（最多 %d token）；识别请求强制 low 思考。", cfg.VisionInputTokenBudget), time.Now())
		var lastErr error
		for _, candidate := range cfg.VisionModels {
			if !candidate.active() {
				cfg.events.stage(event, "视觉候选跳过", "跳过", candidate.Model, "该候选模型已在配置中停用。", time.Now())
				continue
			}
			projectedInput := estimateTokens(cfg.VisionPrompt) + estimateTokens(contextText) + cfg.VisionImageTokenReserve
			if projectedInput > candidate.ContextBudget || projectedInput > candidate.ContextLimit {
				lastErr = fmt.Errorf("vision model %s skipped: projected context exceeds %d", candidate.Model, candidate.ContextLimit)
				cfg.events.stage(event, "视觉上下文预检", "跳过", candidate.Model, fmt.Sprintf("预测输入 %d token，超过工作预算 %d 或总上限 %d，未发送请求。", projectedInput, candidate.ContextBudget, candidate.ContextLimit), time.Now())
				continue
			}
			candidateStarted := time.Now()
			request := makeVisionRequest(candidate.Model, cfg.VisionPrompt, contextText, asset.URL)
			cfg.events.stage(event, "视觉候选调用", "进行中", candidate.Model, fmt.Sprintf("启动 CPA Host 视觉流；取得 stream ID 后启用 %d 秒可取消预算，取消确认最多等待 %d 秒。", candidate.TimeoutSeconds, cfg.VisionCancelGraceSeconds), candidateStarted)
			description, err := hostExecuteVisionStreamWithTimeout(callbackID, lowThinkingModel(candidate.Model), request, candidate.TimeoutSeconds, cfg.VisionCancelGraceSeconds)
			if err != nil {
				lastErr = err
				if isVisionCancellationUnconfirmed(err) {
					cfg.events.stage(event, "视觉取消确认", "失败", candidate.Model, "超时后未能确认上游流已结束；为避免重叠调用和重复计费，已停止本次视觉回退："+err.Error(), candidateStarted)
					return "", err
				}
				detail := "调用失败，继续尝试下一个候选：" + err.Error()
				if isVisionStreamTimeout(err) {
					detail = "可取消识别阶段超时，已确认关闭上游流；安全尝试下一个候选。"
				}
				cfg.events.stage(event, "视觉候选调用", "失败", candidate.Model, detail, candidateStarted)
				continue
			}
			if description == "" {
				lastErr = fmt.Errorf("vision model %s returned no usable text", candidate.Model)
				cfg.events.stage(event, "视觉候选调用", "失败", candidate.Model, "返回为空，继续尝试下一个候选。", candidateStarted)
				continue
			}
			cfg.cache.set(key, "vision", description, cacheTTL(cfg))
			cfg.events.stage(event, "视觉识别完成", "完成", candidate.Model, "已提取视觉记忆：\n"+description, candidateStarted)
			cfg.events.stage(event, "注入视觉记忆", "完成", cfg.PrimaryModel, "原始图片片段已替换为上方视觉记忆文本，随后继续由主文本模型完成任务。", time.Now())
			return description, nil
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no enabled visual model is configured")
		}
		return "", lastErr
	})
	if joined && err == nil {
		cfg.events.stage(event, "合并重复识图请求", "完成", "缓存", "相同图片正在识别，本请求复用了同一任务。", time.Now())
	}
	return value, err
}

func textModels(cfg runtimeConfig) []string {
	return uniqueModels(append([]string{cfg.PrimaryModel}, cfg.TextFallbackModels...))
}
