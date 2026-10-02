package config

// 厂商预设: 把 base_url 与默认模型固化下来, 让 config.toml 保持极短。
//
// 手抄 base_url 极易出错 —— 各家路径前缀都不同 (/v1、/compatible-mode/v1、
// /api/paas/v4、/api/v3)。预设放在 config 包而不是 llm 包, 是因为它本质是配置
// 数据; 放 llm 会让 config 与 llm 互相导入。
type Preset struct {
	Name     string
	Display  string
	Provider string
	BaseURL  string
	Model    string
	Note     string
	// RequiresKey 必须显式声明。
	//
	// 不能像其它字段那样"不写就靠预设默认值": bool 的零值是 false, 漏写会静默变成
	// "该厂商无需密钥", 后果是界面允许切换到无 Key 的云端配置, 调用时才报 401。
	// TestPresetRequiresKeyExplicitly 守住这一点。
	RequiresKey bool
}

var Presets = map[string]Preset{
	"deepseek": {
		Name: "deepseek", Display: "DeepSeek", Provider: ProviderOpenAICompat,
		BaseURL: "https://api.deepseek.com", Model: "deepseek-chat",
		RequiresKey: true,
		Note:        "中文理解好、便宜, 支持 response_format=json_object",
	},
	"qwen": {
		Name: "qwen", Display: "通义千问 (DashScope 兼容模式)", Provider: ProviderOpenAICompat,
		BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Model: "qwen-plus",
		RequiresKey: true,
	},
	"kimi": {
		Name: "kimi", Display: "月之暗面 Kimi", Provider: ProviderOpenAICompat,
		BaseURL: "https://api.moonshot.cn/v1", Model: "moonshot-v1-32k",
		RequiresKey: true,
	},
	"glm": {
		Name: "glm", Display: "智谱 GLM", Provider: ProviderOpenAICompat,
		BaseURL: "https://open.bigmodel.cn/api/paas/v4", Model: "glm-4-plus",
		RequiresKey: true,
	},
	"doubao": {
		Name: "doubao", Display: "火山方舟 豆包", Provider: ProviderOpenAICompat,
		BaseURL: "https://ark.cn-beijing.volces.com/api/v3", Model: "doubao-pro-32k",
		RequiresKey: true,
		Note:        "model 需填方舟上的接入点 ID (ep-xxxx)",
	},
	"openai": {
		Name: "openai", Display: "OpenAI", Provider: ProviderOpenAICompat,
		BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini",
		RequiresKey: true,
	},
	"openrouter": {
		Name: "openrouter", Display: "OpenRouter", Provider: ProviderOpenAICompat,
		BaseURL: "https://openrouter.ai/api/v1", Model: "deepseek/deepseek-chat",
		RequiresKey: true,
	},
	"siliconflow": {
		Name: "siliconflow", Display: "硅基流动", Provider: ProviderOpenAICompat,
		BaseURL: "https://api.siliconflow.cn/v1", Model: "Qwen/Qwen2.5-14B-Instruct",
		RequiresKey: true,
	},
	"azure_openai": {
		Name: "azure_openai", Display: "Azure OpenAI", Provider: ProviderAzureOpenAI,
		BaseURL: "", Model: "",
		RequiresKey: true,
		Note:        "base_url 填 https://<资源名>.openai.azure.com, model 填部署名",
	},
	// 本地 / 内网: 无需 key。备份场景数据不出网, 这几个很关键。
	"ollama": {
		Name: "ollama", Display: "本地 Ollama", Provider: ProviderOpenAICompat,
		BaseURL: "http://127.0.0.1:11434/v1", Model: "qwen2.5:14b",
		RequiresKey: false, Note: "数据不出网; 建议 >=14B 模型",
	},
	"vllm": {
		Name: "vllm", Display: "本地 vLLM", Provider: ProviderOpenAICompat,
		BaseURL: "http://127.0.0.1:8000/v1", Model: "",
		RequiresKey: false, Note: "model 填 vLLM 启动时的模型名",
	},
	"lmstudio": {
		Name: "lmstudio", Display: "本地 LM Studio", Provider: ProviderOpenAICompat,
		BaseURL: "http://127.0.0.1:1234/v1", Model: "",
		RequiresKey: false,
	},
}

// FindPreset 按名字取预设。
func FindPreset(name string) (Preset, bool) {
	preset, ok := Presets[name]
	return preset, ok
}

// PresetNames 返回全部预设名 (已排序)。
func PresetNames() []string {
	names := make([]string, 0, len(Presets))
	for name := range Presets {
		names = append(names, name)
	}
	return sortedStrings(names)
}
