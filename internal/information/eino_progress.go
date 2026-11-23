package information

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
)

const loopCorrectionPrefix = "[LOOP CORRECTION] "

type informationProgress struct {
	RoundID  uint64
	Evidence [3]int64
	NoChange int
	Warned   bool
}

func (h *informationHost) reviewProgress(ctx context.Context) (string, bool, error) {
	var progress informationProgress
	_ = json.Unmarshal([]byte(h.request.RouteProgressJSON), &progress)
	var rounds []Step
	db := h.service.DB.WithContext(ctx)
	if err := db.Select("id, result_json").Where("request_id = ? AND eino_epoch = ? AND kind = ? AND stage = ?", h.owner.RequestID, h.request.EinoEpoch, "eino_model", "succeeded").Order("id DESC").Limit(12).Find(&rounds).Error; err != nil {
		return "", false, err
	}
	if len(rounds) == 0 || rounds[0].ID == progress.RoundID {
		return "", false, nil
	}
	var evidence [3]int64
	if err := db.Model(&taskDocument{}).Where("request_id = ? AND status = ? AND body_valid = ? AND evidence_digest <> ''", h.owner.RequestID, "succeeded", true).Distinct("evidence_digest").Count(&evidence[0]).Error; err != nil {
		return "", false, err
	}
	if err := db.Table("information_observations o").Joins("JOIN information_task_observations t ON t.observation_id = o.id").Where("t.request_id = ?", h.owner.RequestID).Select("COUNT(DISTINCT SHA2(o.fields_json, 256))").Scan(&evidence[1]).Error; err != nil {
		return "", false, err
	}
	if err := db.Model(&DiscoveredSource{}).Where("discovery_request_id = ? AND state = ?", h.owner.RequestID, "verified").Count(&evidence[2]).Error; err != nil {
		return "", false, err
	}
	var actions []string
	var thinking []string
	for _, round := range rounds {
		var proposal aiapp.ChatResponse
		if json.Unmarshal([]byte(round.ResultJSON), &proposal) != nil {
			continue
		}
		keys := make([]string, 0, len(proposal.Message.ToolCalls))
		for _, call := range proposal.Message.ToolCalls {
			args, err := informationBusinessArgs(call.Function.Arguments, call.Function.Name)
			if err != nil {
				args = call.Function.Arguments
			}
			keys = append(keys, informationToolInput(call.Function.Name, args, ""))
		}
		sort.Strings(keys)
		actions = append(actions, agentdata.Hash(keys))
		thinking = append(thinking, strings.TrimSpace(proposal.Message.ReasoningContent))
	}
	similar := false
	if len(thinking) >= 2 && thinking[0] != "" && thinking[1] != "" && h.service.Crawler != nil && h.service.Crawler.LoopEmbedding != nil {
		vectors, err := h.service.Crawler.LoopEmbedding.EmbedTexts(ctx, thinking[:2])
		if err == nil && len(vectors) == 2 {
			similar = reasoningSimilarity(vectors[0], vectors[1]) >= 0.92
		}
	}
	repeating := false
	if len(actions) > 1 {
		for _, earlier := range actions[1:] {
			repeating = repeating || actions[0] == earlier
		}
	}
	progress.RoundID = rounds[0].ID
	var correction string
	var stop bool
	if progress.Evidence != evidence {
		progress.Evidence, progress.NoChange, progress.Warned = evidence, 0, false
	} else {
		progress.NoChange++
		stop = progress.Warned && progress.NoChange >= 2 && repeating
		if !stop && (progress.NoChange >= 2 || similar && repeating) {
			correction = loopCorrectionPrefix + "Recent steps produced no new sources or records. Use each failed target's fix or recovery steps. If the source is captured, extract all still-missing fields from its source_ref in one pass. If a required rule is absent in that snapshot, check a different source that can substantiate it, or submit as partial with that specific gap."
			progress.Warned = true
		}
	}
	h.request.RouteProgressJSON = string(mustMarshal(progress))
	return correction, stop, nil
}

func reasoningSimilarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, aa, bb float64
	for i, v := range a {
		x, y := float64(v), float64(b[i])
		dot += x * y
		aa += x * x
		bb += y * y
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return dot / math.Sqrt(aa*bb)
}
