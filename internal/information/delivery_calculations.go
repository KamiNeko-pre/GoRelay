package information

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type CalculationInput struct {
	Value     float64         `json:"value"`
	Quote     string          `json:"quote" jsonschema:"description=原文中的完整短句或表格行，必须包含此数值及其计费口径或单位。"`
	Reference StoredReference `json:"reference"`
}

type DeliveryCalculation struct {
	ID             string                              `json:"id"`
	Formula        string                              `json:"formula" jsonschema:"description=引用已声明输入或前面的计算 ID，支持四则运算及 min max ceil floor round pow。"`
	Description    string                              `json:"description,omitempty"`
	Unit           string                              `json:"unit,omitempty"`
	Inputs         map[string]CalculationInput         `json:"inputs,omitempty"`
	EvidenceInputs map[string]EvidenceCalculationInput `json:"evidence_inputs,omitempty"`
	PromptInputs   map[string]PromptCalculationInput   `json:"prompt_inputs,omitempty"`
}

type EvidenceCalculationInput struct {
	EvidenceID  string                   `json:"evidence_id,omitempty" jsonschema:"description=已返回的 matches[].evidence_id。与 source_value 二选一。"`
	SourceValue *JSONCalculationEvidence `json:"source_value,omitempty" jsonschema:"description=原文 JSON 中已有确切路径时，直接绑定已保存的快照和路径。"`
	NumberIndex *int                     `json:"number_index,omitempty" jsonschema:"minimum=0,description=该片段 numbers 数组的索引，从 0 开始。由宿主取原数，无需重抄 value。"`
	Value       *float64                 `json:"value,omitempty" jsonschema:"description=兼容的原始数值写法。未提供 number_index 时填写。"`
	Label       string                   `json:"label,omitempty"`
	Unit        string                   `json:"unit,omitempty"`
}

type JSONCalculationEvidence struct {
	SourceRef   luaSourceRef `json:"source_ref"`
	JSONPointer string       `json:"json_pointer" jsonschema:"description=原始 JSON 的绝对 JSON Pointer。复用采集或 Lua 已观察到的路径。"`
}

type PromptCalculationInput struct {
	Value float64 `json:"value" jsonschema:"description=题面中的原始量，例如题面写 5 TB 就传 5；换算不要在这里完成。"`
	Label string  `json:"label"`
}

type DeliveryCalculationInputs struct {
	PromptIndices  map[string]int                      `json:"prompt_indices,omitempty" jsonschema:"description=公式变量到宿主 prompt_quantities[].index 的绑定。使用题面原数，在公式中换算单位。"`
	EvidenceInputs map[string]EvidenceCalculationInput `json:"evidence_inputs,omitempty" jsonschema:"description=公式变量到可复核的原文数字的绑定。"`
}

type calculationSources struct {
	prompt       string
	evidence     func(string) (string, StoredReference, error)
	structured   func(JSONCalculationEvidence) (float64, StoredReference, error)
	results      map[string]float64
	sharedInputs map[string]float64
}

var formulaPlaceholder = regexp.MustCompile(`\{\{([A-Za-z_][A-Za-z0-9_]*)\}\}`)

var explicitDerivationCue = regexp.MustCompile(`(?i)(?:\b(?:calculate|compute|total\s+cost|monthly\s+cost|annual\s+cost|yearly\s+cost|total\s+price|total\s+fee|total\s+bill|monthly\s+bill|cost\s+of\s+processing|combined\s+[^.!?\n]*capacity)\b|\bestimate\b[^.!?\n]*\b(?:cost|costs|fee|fees|bill|bills|price|total|monthly|annual|charges?)\b|计算|估算|核算|测算|折算|总成本|总费用|月成本|月费用|月均费用|年成本|年费用|总价|合计费用|花多少钱|共需)`)
var billingScaleUnitCue = regexp.MustCompile(`(?i)(?:\b(?:tb|gb|gib|mib|mb|kb|pb|tokens?|requests?)\b|万次|亿次|次)`)
var costComparisonCue = regexp.MustCompile(`(?i)(?:\b(?:cost|costs|bill|bills|fee|fees|spend|cheapest|most\s+cost[- ]effective)\b|费用|花费|成本|账单|总计|合计|多少钱|最划算|最便宜|更省钱)`)

// taskRequiresCalculation infers from immutable task requirements whether the
// caller asks for derived numeric computation, independent of WorkPlan flags.
func taskRequiresCalculation(requirements TaskRequirements) bool {
	parts := make([]string, 0, 1+len(requirements.Additions))
	if trimmed := strings.TrimSpace(requirements.OriginalText); trimmed != "" {
		parts = append(parts, trimmed)
	}
	for _, addition := range requirements.Additions {
		if trimmed := strings.TrimSpace(addition); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	if len(parts) == 0 {
		return false
	}
	text := strings.Join(parts, "\n")
	if explicitDerivationCue.MatchString(text) {
		return true
	}
	return len(parsedPromptQuantities(text)) > 0 && billingScaleUnitCue.MatchString(text) && costComparisonCue.MatchString(text)
}

var deliveryNumber = regexp.MustCompile(`[-+]?(?:[0-9][0-9,]*(?:\.[0-9]+)?|\.[0-9]+)(?:[eE][-+]?[0-9]+)?`)

func verifyCalculationInputAny(input CalculationInput, bodies []string) error {
	quote := strings.Join(strings.Fields(input.Quote), " ")
	if quote == "" {
		return fmt.Errorf("所引原文在该快照中找不到")
	}
	foundQuote := false
	for _, body := range bodies {
		if body == "" {
			continue
		}
		if strings.Contains(body, quote) || strings.Contains(strings.Join(strings.Fields(body), " "), quote) {
			foundQuote = true
			break
		}
	}
	if !foundQuote {
		return fmt.Errorf("所引原文在该快照中找不到")
	}
	for _, v := range extractDeliveryNumbers(quote) {
		n, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimRight(v, ","), ",", ""), 64)
		if err == nil && n == input.Value {
			return nil
		}
	}
	return fmt.Errorf("原文不含数值 %s", strconv.FormatFloat(input.Value, 'g', -1, 64))
}

func verifyCalculationInput(input CalculationInput, body string) error {
	return verifyCalculationInputAny(input, []string{body})
}

func verifyDeliveryCalculations(draft *DeliveryDraft, resolve func(StoredReference) (string, error)) error {
	return verifyDeliveryCalculationsWithSources(draft, func(ref StoredReference) ([]string, error) {
		if resolve == nil {
			return nil, fmt.Errorf("snapshot resolver unavailable")
		}
		body, err := resolve(ref)
		if err != nil {
			return nil, err
		}
		return []string{body}, nil
	})
}

func verifyDeliveryCalculationsWithSources(draft *DeliveryDraft, resolve func(StoredReference) ([]string, error), source ...calculationSources) error {
	if len(draft.Calculations) == 0 {
		return nil
	}
	values := map[string]float64{}
	failedIDs := map[string]bool{}
	var failures []string
	var pendingRefs []StoredReference
	var passed []string
	var sources calculationSources
	if len(source) > 0 {
		sources = source[0]
	}
	quantities := parsedPromptQuantities(sources.prompt)
	promptValues := make(map[float64]bool, len(quantities))
	for _, qty := range quantities {
		promptValues[qty.Value] = true
	}
	resolveEvidence := func(input EvidenceCalculationInput) (float64, error) {
		if input.SourceValue != nil {
			if input.EvidenceID != "" || input.NumberIndex != nil {
				return 0, fmt.Errorf("source_value 与 evidence_id/number_index 二选一")
			}
			if sources.structured == nil {
				return 0, fmt.Errorf("结构化证据解析不可用")
			}
			value, ref, err := sources.structured(*input.SourceValue)
			if err != nil {
				return 0, err
			}
			if input.Value != nil && *input.Value != value {
				return 0, fmt.Errorf("原路径取到 %g，与填写的原数 %g 不符；用实际路径，在公式中换算", value, *input.Value)
			}
			pendingRefs = append(pendingRefs, ref)
			return value, nil
		}
		if sources.evidence == nil {
			return 0, fmt.Errorf("证据句柄解析不可用")
		}
		body, ref, err := sources.evidence(input.EvidenceID)
		if err != nil {
			return 0, err
		}
		value, err := deliveryEvidenceInputValue(input, body)
		if err == nil {
			pendingRefs = append(pendingRefs, ref)
		}
		return value, err
	}
	shared := map[string]float64{}
	if bindings := draft.CalculationInputs; bindings != nil {
		addShared := func(name string, value float64, err error) {
			if _, exists := shared[name]; !token.IsIdentifier(name) || exists {
				err = fmt.Errorf("变量名必须是唯一标识符且只声明一个来源")
			}
			if err != nil {
				failures = append(failures, fmt.Sprintf("共用输入 %q: %v", name, err))
				return
			}
			shared[name] = value
		}
		for name, index := range bindings.PromptIndices {
			if index < 0 || index >= len(quantities) {
				addShared(name, 0, fmt.Errorf("题面数值索引 %d 无效；选择 prompt_quantities 中的原数索引", index))
				continue
			}
			addShared(name, quantities[index].Value, nil)
		}
		for name, input := range bindings.EvidenceInputs {
			value, err := resolveEvidence(input)
			addShared(name, value, err)
			if err == nil {
				input.Value = &value
				bindings.EvidenceInputs[name] = input
			}
		}
	}

	for _, calc := range draft.Calculations {
		addFailure := func(msg string) {
			failures = append(failures, fmt.Sprintf("计算 %q: %s", calc.ID, msg))
		}
		_, exists := values[calc.ID]
		if _, bound := shared[calc.ID]; bound {
			addFailure("计算 ID 与共用输入重名")
			continue
		}
		if !token.IsIdentifier(calc.ID) || exists || failedIDs[calc.ID] {
			addFailure("ID 必须是唯一标识符")
			if calc.ID != "" {
				failedIDs[calc.ID] = true
			}
			continue
		}
		inputNames := make([]string, 0, len(calc.Inputs))
		for name := range calc.Inputs {
			inputNames = append(inputNames, name)
		}
		sort.Strings(inputNames)
		inputs := maps.Clone(shared)
		calcFailed := false
		addInput := func(name string, value float64, err error) {
			if _, exists := inputs[name]; exists {
				err = fmt.Errorf("同一变量必须只声明一个来源")
			}
			if err != nil {
				calcFailed = true
				addFailure(fmt.Sprintf("输入 %q: %v", name, err))
				return
			}
			inputs[name] = value
		}
		for _, name := range inputNames {
			input := calc.Inputs[name]
			bodies, err := resolve(input.Reference)
			if err == nil {
				err = verifyCalculationInputAny(input, bodies)
			}
			addInput(name, input.Value, err)
			if err == nil {
				pendingRefs = append(pendingRefs, input.Reference)
			}
		}
		for name, input := range calc.PromptInputs {
			var err error
			if !promptValues[input.Value] {
				err = fmt.Errorf("原始任务未给出数值 %g；从 prompt_quantities 取题面原数并在 formula 中换算，外部取值才用 evidence_inputs", input.Value)
			}
			addInput(name, input.Value, err)
		}
		for name, input := range calc.EvidenceInputs {
			value, err := resolveEvidence(input)
			addInput(name, value, err)
			if err == nil {
				input.Value = &value
				calc.EvidenceInputs[name] = input
			}
		}
		if calcFailed {
			failedIDs[calc.ID] = true
			continue
		}
		expr, err := parser.ParseExpr(formulaPlaceholder.ReplaceAllString(calc.Formula, "$1"))
		if err == nil && len(source) > 0 {
			err = validateDeliveryFormulaLiterals(expr, inputs)
		}
		if err != nil {
			failedIDs[calc.ID] = true
			addFailure(err.Error())
			continue
		}
		v, err := evaluateDeliveryFormula(expr, inputs, values)
		if err != nil {
			failedIDs[calc.ID] = true
			if !formulaFailedOnlyOnUpstream(err, failedIDs) {
				addFailure(err.Error())
			}
			continue
		}
		if math.IsInf(v, 0) || math.IsNaN(v) {
			failedIDs[calc.ID] = true
			addFailure("结果不是有限数值，请检查分母和取值范围")
			continue
		}
		values[calc.ID] = v
		passed = append(passed, calc.ID)
	}
	if len(failures) > 0 {
		failure := businessError("DELIVERY_CALCULATION_INVALID", "delivery",
			fmt.Sprintf("发现 %d 处计算校验错误：\n- %s", len(failures), strings.Join(failures, "\n- ")),
			"保留 passed_ids，修正失败的输入或公式。用 calculation_inputs 将共享变量绑定到 evidence_id 和原文数字位置，或绑定到 prompt_quantities 的索引。无法证实时以 partial 说明缺口。")
		failure.Details = map[string]any{"passed_ids": passed, "failures": failures, "prompt_quantities": quantities}
		return failure
	}
	for id, value := range values {
		if sources.results != nil {
			sources.results[id] = value
		}
	}
	if sources.sharedInputs != nil {
		maps.Copy(sources.sharedInputs, shared)
	}
	sort.Slice(pendingRefs, func(i, j int) bool { return string(mustMarshal(pendingRefs[i])) < string(mustMarshal(pendingRefs[j])) })
	draft.References = append(draft.References, pendingRefs...)
	for id, v := range values {
		placeholder := "{{" + id + "}}"
		display := strconv.FormatFloat(v, 'f', -1, 64)
		draft.Report = strings.ReplaceAll(draft.Report, placeholder, display)
		for _, table := range draft.Tables {
			for _, row := range table.Rows {
				for field, datum := range row {
					s, ok := datum.(string)
					if !ok {
						continue
					}
					if s == placeholder {
						row[field] = v
					} else {
						row[field] = strings.ReplaceAll(s, placeholder, display)
					}
				}
			}
		}
	}
	if strings.Contains(draft.Report, "{{calculations:details}}") {
		draft.Report = strings.ReplaceAll(draft.Report, "{{calculations:details}}", deliveryCalculationDetails(draft.Calculations, values, shared))
	}
	return nil
}

func deliveryEvidenceInputValue(input EvidenceCalculationInput, body string) (float64, error) {
	numbers := extractDeliveryNumbers(body)
	if input.NumberIndex != nil {
		i := *input.NumberIndex
		if i < 0 || i >= len(numbers) {
			return 0, fmt.Errorf("%s 的 number_index=%d 超出 numbers 范围；从这组原数选择: %v", input.EvidenceID, i, numbers)
		}
		value, err := strconv.ParseFloat(strings.ReplaceAll(numbers[i], ",", ""), 64)
		if err != nil || math.IsInf(value, 0) || (input.Value != nil && *input.Value != value) {
			return 0, fmt.Errorf("%s 的 numbers[%d]=%s 与输入不一致；只保留准确索引，在公式中换算", input.EvidenceID, i, numbers[i])
		}
		return value, nil
	}
	if input.Value == nil {
		return 0, fmt.Errorf("%s 还未选择数值；填入 numbers 的索引 number_index，或原样填入 value。可用原数: %v", input.EvidenceID, numbers)
	}
	value := *input.Value
	if containsDeliveryNumber(body, value) {
		return value, nil
	}
	if value == 0 {
		return 0, fmt.Errorf("%s 不包含原数 0（可用原数: %v）；纯文字免除规则请引用对应文档并在 report 解释", input.EvidenceID, numbers)
	}
	return 0, fmt.Errorf("%s 不包含原数 %g；可用原数: %v。选择对应的 number_index，在 formula 中换算", input.EvidenceID, value, numbers)
}

func deliveryCalculationDetails(calculations []DeliveryCalculation, values, shared map[string]float64) string {
	var details strings.Builder
	if len(shared) > 0 {
		var operands []string
		for name, value := range shared {
			operands = append(operands, fmt.Sprintf("%s=%g", name, value))
		}
		sort.Strings(operands)
		fmt.Fprintf(&details, "- 共用输入：%s\n", strings.Join(operands, "，"))
	}
	for _, calc := range calculations {
		label := strings.TrimSpace(calc.Description)
		if label == "" {
			label = calc.ID
		}
		fmt.Fprintf(&details, "- %s：`%s` = %s", label, formulaPlaceholder.ReplaceAllString(calc.Formula, "$1"), strconv.FormatFloat(values[calc.ID], 'f', -1, 64))
		if calc.Unit != "" {
			fmt.Fprintf(&details, " %s", calc.Unit)
		}
		var operands []string
		for name, input := range calc.Inputs {
			operands = append(operands, fmt.Sprintf("%s=%g", name, input.Value))
		}
		for name, input := range calc.EvidenceInputs {
			operands = append(operands, strings.TrimSpace(fmt.Sprintf("%s=%g %s", name, *input.Value, input.Unit)))
		}
		for name, input := range calc.PromptInputs {
			operands = append(operands, fmt.Sprintf("%s=%g", name, input.Value))
		}
		if len(operands) > 0 {
			sort.Strings(operands)
			fmt.Fprintf(&details, "（%s）", strings.Join(operands, "，"))
		}
		details.WriteByte('\n')
	}
	return strings.TrimSpace(details.String())
}

var promptQuantity = regexp.MustCompile(`(?i)([-+]?(?:[0-9][0-9,]*(?:\.[0-9]+)?|\.[0-9]+)(?:e[-+]?[0-9]+)?)(?:\s*(万|亿)|\s*(thousand|million|billion|trillion)\b|([kmbt])\b)?`)

type promptQuantityValue struct {
	Index    int     `json:"index"`
	Original string  `json:"original"`
	Value    float64 `json:"value"`
}

func isHyphenSeparator(text string, matchStart int, raw string) bool {
	if matchStart <= 0 || len(raw) == 0 || (raw[0] != '-' && raw[0] != '+') {
		return false
	}
	prev, _ := utf8.DecodeLastRuneInString(text[:matchStart])
	return unicode.IsLetter(prev) || unicode.IsDigit(prev)
}

func trimNumericPrefixSign(text string, start, end int) string {
	raw := strings.TrimRight(text[start:end], ",")
	if isHyphenSeparator(text, start, raw) {
		raw = strings.TrimLeft(raw, "-+")
	}
	return raw
}

// extractDeliveryNumbers returns numeric tokens from prose while avoiding
// false negative signs inside hyphenated identifiers or ranges (e.g. us-east-1, 30-days).
func extractDeliveryNumbers(text string) []string {
	indices := deliveryNumber.FindAllStringIndex(text, -1)
	if len(indices) == 0 {
		return nil
	}
	out := make([]string, 0, len(indices))
	for _, idx := range indices {
		if token := trimNumericPrefixSign(text, idx[0], idx[1]); token != "" {
			out = append(out, token)
		}
	}
	return out
}

func parsedPromptQuantities(prompt string) []promptQuantityValue {
	var quantities []promptQuantityValue
	seen := map[float64]bool{}
	for _, idx := range promptQuantity.FindAllStringSubmatchIndex(prompt, -1) {
		matchStart, matchEnd := idx[0], idx[1]
		numStart, numEnd := idx[2], idx[3]
		if numStart < 0 || numEnd <= numStart {
			continue
		}
		// Skip numbers directly attached to identifier hyphens (e.g. us-east-1, L4-A).
		if numStart > 0 && prompt[numStart-1] == '-' && isHyphenSeparator(prompt, numStart, prompt[numStart:numEnd]) {
			prevBeforeHyphen, _ := utf8.DecodeLastRuneInString(prompt[:numStart-1])
			if unicode.IsLetter(prevBeforeHyphen) {
				continue
			}
		}
		rawNum := trimNumericPrefixSign(prompt, numStart, numEnd)
		n, err := strconv.ParseFloat(strings.TrimRight(strings.ReplaceAll(rawNum, ",", ""), "."), 64)
		if err != nil {
			continue
		}
		suffix := ""
		for pair := 4; pair+1 < len(idx); pair += 2 {
			if idx[pair] >= 0 && idx[pair+1] > idx[pair] {
				suffix = strings.ToLower(prompt[idx[pair]:idx[pair+1]])
				break
			}
		}
		switch suffix {
		case "k", "thousand":
			n *= 1e3
		case "万":
			n *= 1e4
		case "m", "million":
			n *= 1e6
		case "亿":
			n *= 1e8
		case "b", "billion":
			n *= 1e9
		case "t", "trillion":
			n *= 1e12
		}
		if math.IsInf(n, 0) || seen[n] {
			continue
		}
		seen[n] = true
		orig := strings.TrimSpace(prompt[matchStart:matchEnd])
		if isHyphenSeparator(prompt, matchStart, orig) {
			orig = strings.TrimLeft(orig, "-+")
		}
		quantities = append(quantities, promptQuantityValue{Index: len(quantities), Original: orig, Value: n})
	}
	return quantities
}

func containsDeliveryNumber(text string, value float64) bool {
	for _, raw := range extractDeliveryNumbers(text) {
		n, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", ""), 64)
		if err == nil && n == value {
			return true
		}
	}
	return false
}

func isAllowedFormulaLiteral(raw string) bool {
	// Reject decimal literals with fractional parts (e.g. 0.023, 6.95, 1.5).
	if strings.Contains(raw, ".") && !strings.ContainsAny(raw, "eE") {
		return false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || math.Trunc(v) != v {
		return false
	}
	n := int64(v)
	switch n {
	case 0, 1, 2, 4, 7, 10, 12, 24, 30, 52, 60, 100, 365, 3600, 1024, 1048576, 1073741824:
		return true // Arithmetic identities, standard calendar/time factors, and binary byte steps.
	}
	// Accept exact powers of 10 from 10^1 up to 10^12 in either decimal or 1eN notation.
	if n >= 10 && n <= 1_000_000_000_000 {
		for p := int64(10); p <= n; p *= 10 {
			if p == n {
				return true
			}
			if p > math.MaxInt64/10 {
				break
			}
		}
	}
	return false
}

func isAllowedPowerExponent(node ast.Node, powExponents map[*ast.BasicLit]bool) bool {
	lit, ok := node.(*ast.BasicLit)
	if !ok || !powExponents[lit] {
		return false
	}
	v, err := strconv.ParseFloat(lit.Value, 64)
	return err == nil && v >= 0 && v <= 60 && math.Trunc(v) == v && !strings.Contains(lit.Value, ".")
}

func validateDeliveryFormulaLiterals(expr ast.Expr, verifiedInputs map[string]float64) error {
	matchedBindings := make(map[float64]int, len(verifiedInputs))
	for _, value := range verifiedInputs {
		matchedBindings[value]++
	}
	powExponents := map[*ast.BasicLit]bool{}
	ast.Inspect(expr, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != "pow" {
			return true
		}
		baseLit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || (baseLit.Value != "2" && baseLit.Value != "10") {
			return true
		}
		if expLit, ok := call.Args[1].(*ast.BasicLit); ok {
			powExponents[expLit] = true
		}
		return true
	})
	var invalid string
	ast.Inspect(expr, func(node ast.Node) bool {
		lit, ok := node.(*ast.BasicLit)
		if !ok || invalid != "" {
			return invalid == ""
		}
		if isAllowedFormulaLiteral(lit.Value) || isAllowedPowerExponent(lit, powExponents) {
			return true
		}
		if value, err := strconv.ParseFloat(lit.Value, 64); err == nil && matchedBindings[value] == 1 {
			return true
		}
		invalid = lit.Value
		return false
	})
	if invalid != "" {
		return fmt.Errorf("字面量 %s 缺少来源；用 evidence_inputs 或 prompt_inputs 声明业务数值", invalid)
	}
	return nil
}

func evaluateDeliveryFormula(expr ast.Expr, inputs, values map[string]float64) (float64, error) {
	unsupported := fmt.Errorf("表达式只支持数值、已声明变量、四则运算及公开数学函数")
	switch x := expr.(type) {
	case *ast.BasicLit:
		return strconv.ParseFloat(x.Value, 64)
	case *ast.Ident:
		if v, ok := inputs[x.Name]; ok {
			return v, nil
		}
		if v, ok := values[x.Name]; ok {
			return v, nil
		}
		return 0, fmt.Errorf("变量 %q 未声明", x.Name)
	case *ast.ParenExpr:
		return evaluateDeliveryFormula(x.X, inputs, values)
	case *ast.UnaryExpr:
		v, err := evaluateDeliveryFormula(x.X, inputs, values)
		if x.Op == token.SUB {
			return -v, err
		}
		if x.Op == token.ADD {
			return v, err
		}
	case *ast.BinaryExpr:
		a, err := evaluateDeliveryFormula(x.X, inputs, values)
		if err != nil {
			return 0, err
		}
		b, err := evaluateDeliveryFormula(x.Y, inputs, values)
		if err != nil {
			return 0, err
		}
		switch x.Op {
		case token.ADD:
			return a + b, nil
		case token.SUB:
			return a - b, nil
		case token.MUL:
			return a * b, nil
		case token.QUO:
			if b == 0 {
				return 0, fmt.Errorf("分母为零")
			}
			return a / b, nil
		}
	case *ast.CallExpr:
		if fn, ok := x.Fun.(*ast.Ident); ok && !x.Ellipsis.IsValid() {
			if f := unaryCalculation[fn.Name]; f != nil && len(x.Args) == 1 {
				a, err := evaluateDeliveryFormula(x.Args[0], inputs, values)
				return f(a), err
			}
			if f := binaryCalculation[fn.Name]; f != nil && len(x.Args) == 2 {
				a, err := evaluateDeliveryFormula(x.Args[0], inputs, values)
				if err != nil {
					return 0, err
				}
				b, err := evaluateDeliveryFormula(x.Args[1], inputs, values)
				return f(a, b), err
			}
			return 0, fmt.Errorf("函数 %q 不支持这些参数", fn.Name)
		}
	}
	return 0, unsupported
}

var unaryCalculation = map[string]func(float64) float64{"ceil": math.Ceil, "floor": math.Floor, "round": math.Round}
var binaryCalculation = map[string]func(float64, float64) float64{"min": math.Min, "max": math.Max, "pow": math.Pow}

func formulaFailedOnlyOnUpstream(err error, failedIDs map[string]bool) bool {
	if err == nil || len(failedIDs) == 0 {
		return false
	}
	msg := err.Error()
	for id := range failedIDs {
		if strings.Contains(msg, fmt.Sprintf("变量 %q 未声明", id)) {
			return true
		}
	}
	return false
}
