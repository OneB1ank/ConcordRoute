package service

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
)

// 仅识别标准 token 阶梯；Fast/Flex 与缓存价格由已有计费引擎组合，避免重复乘倍率。
var aboveTierPricePattern = regexp.MustCompile(`^(input|output)_cost_per_token_above_([1-9]\d*)k_tokens$`)

// deriveLongContextFromAboveTierFields 将目录绝对价转成内部阈值与倍率。
// 参考上游 530fb20f2 的解析契约；单阶梯结构取最小阈值，不混合不同阈值的输入/输出价格。
func deriveLongContextFromAboveTierFields(rawEntry json.RawMessage, pricing *LiteLLMModelPricing) {
	if pricing == nil || !bytes.Contains(rawEntry, []byte("_above_")) {
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rawEntry, &fields); err != nil {
		return
	}
	type tierPrices struct{ input, output float64 }
	tiers := make(map[int]*tierPrices)
	for key, value := range fields {
		match := aboveTierPricePattern.FindStringSubmatch(key)
		if match == nil {
			continue
		}
		var price float64
		if err := json.Unmarshal(value, &price); err != nil || price <= 0 || math.IsInf(price, 0) || math.IsNaN(price) {
			continue
		}
		thousands, err := strconv.Atoi(match[2])
		// 外部目录的阈值先检查乘法溢出，避免异常字段绕回负数或过小值。
		if err != nil || thousands > math.MaxInt/1000 {
			continue
		}
		threshold := thousands * 1000
		tier := tiers[threshold]
		if tier == nil {
			tier = &tierPrices{}
			tiers[threshold] = tier
		}
		if match[1] == "input" {
			tier.input = price
		} else {
			tier.output = price
		}
	}
	threshold := 0
	for candidate := range tiers {
		if threshold == 0 || candidate < threshold {
			threshold = candidate
		}
	}
	if threshold == 0 {
		return
	}
	tier := tiers[threshold]
	inputMultiplier, outputMultiplier := 1.0, 1.0
	if tier.input > 0 && pricing.InputCostPerToken > 0 {
		inputMultiplier = tier.input / pricing.InputCostPerToken
	}
	if tier.output > 0 && pricing.OutputCostPerToken > 0 {
		outputMultiplier = tier.output / pricing.OutputCostPerToken
	}
	// 缺失一侧价格保持 1 倍，拒绝非有限比例；两侧都没有附加费时不启用阶梯。
	if math.IsInf(inputMultiplier, 0) || math.IsInf(outputMultiplier, 0) ||
		math.IsNaN(inputMultiplier) || math.IsNaN(outputMultiplier) ||
		(inputMultiplier <= 1 && outputMultiplier <= 1) {
		return
	}
	pricing.LongContextInputTokenThreshold = threshold
	pricing.LongContextInputCostMultiplier = inputMultiplier
	pricing.LongContextOutputCostMultiplier = outputMultiplier
}
