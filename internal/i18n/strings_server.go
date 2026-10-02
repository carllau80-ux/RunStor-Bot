package i18n

// 服务端文案的英译 (与界面文案同一张表, 只是按模块分文件便于校对)。
//
// 这些字符串会进 API 响应 (报错、提示、状态), 所以必须与服务端逻辑里的字面量一致 ——
// 键就是源码里的原文, 改文案时要一起改。
var serverEnglish = map[string]string{
	"profile 不能为空":    "Profile name is required",
	"session_id 不能为空": "session_id is required",
	"text 不能为空":       "text is required",
	"两者都没有无法登录目标机":    "Neither was provided — cannot log in to the target",
	"主机地址只允许字母、数字、点、短横线、下划线与冒号": "Host addresses may contain letters, digits, dots, dashes, underscores and colons only",
	"会话 %s 计划已定稿":                                  "Session %s plan finalized",
	"会话不存在: ":                                      "Session not found: ",
	"会话不存在或已过期: ":                                  "Session not found or expired: ",
	"信息完整后才能提交":                                    "The plan must be complete before submitting",
	"内置页面缺失":                                       "Built-in page is missing",
	"单次最多导入 100 个文件":                               "At most 100 files per import",
	"单次最多导入 50 份文档":                                "At most 50 documents per import",
	"单次最多巡检 50 台主机":                                "At most 50 hosts per inspection",
	"只支持导入 %s 文件":                                  "Only %s files are supported",
	"只支持导入目录内的 .md / .markdown / .txt 文件":          "Only .md / .markdown / .txt files inside the import directory are supported",
	"只支持导入目录内的相对路径":                                "Only relative paths inside the import directory are supported",
	"可换用能力更强的模型, 或先切 demo 模式":                      "Try a stronger model, or switch to demo mode",
	"可用 profile 见 GET /api/llm/profiles":           "See GET /api/llm/profiles for available profiles",
	"回答里没有标注引用编号, 下面是检索到的片段, 请自行核对":                "The answer carries no citation numbers; the retrieved passages are below — please verify yourself",
	"在 config.toml 的 server.kb.import_dir 指定目录后重启": "Set server.kb.import_dir in config.toml and restart",
	"导入目录不存在: %w":                                  "Import directory does not exist: %w",
	"导入目录无效: %w":                                   "Invalid import directory: %w",
	"导入目录里没有可导入的文件 (只支持 %s)":                       "No importable files in the import directory (only %s)",
	"巡检服务未初始化":                                     "Inspection service is not initialized",
	"巡检服务未启用":                                      "Inspection service is disabled",
	"巡检记录不存在: {0}":                                 "Inspection record not found: {0}",
	"文件不可读: %w":                                    "File is not readable: %w",
	"文档内容为空":                                       "Document is empty",
	"文档数已达上限 %d, 请先删除一些":                           "Document limit %d reached — delete some first",
	"文档超过单份上限 %s (实际 %s)":                          "Document exceeds the per-file limit of %s (actual %s)",
	"文档里没有可索引的内容":                                  "Document has no indexable content",
	"服务内部错误":                                       "Internal error",
	"服务器巡检已关闭":                                     "Server inspection is disabled",
	"服务器目录":                                        "Server directory",
	"未知":                                           "Unknown",
	"未配置可用的模型, 以下是知识库里与问题最相关的片段, 请自行阅读判断。": "No usable model is configured. Below are the passages most relevant to your question — please read and judge them yourself.",
	"未配置服务器导入目录":          "No server import directory configured",
	"模型调用失败, 以下是检索到的片段: ": "Model call failed. Retrieved passages: ",
	"没有可导入的内容":            "Nothing to import",
	"没有可导入的文件":            "No importable files",
	"注意: 回答里引用了不存在的资料编号 %s, 这部分内容没有依据, 请以引用为准": "Note: the answer cites non-existent source numbers %s — that part has no support; rely on the citations",
	"浏览器上传": "Browser upload",
	"知识库总大小将超过上限 %s (当前 %s), 请先删除一些文档": "Total size would exceed the %s limit (currently %s) — delete some documents first",
	"知识库未启用": "Knowledge base is disabled",
	"知识库里没有找到与这个问题相关的片段。可以换个说法 (用文档里的术语), 或者先把相关资料导入知识库。": "No passages related to this question were found. Try different wording (using the terminology in your documents), or import the relevant documents first.",
	"粘贴文本":     "Pasted text",
	"缺少请求体":    "Missing request body",
	"网页改配置已关闭": "Web configuration is disabled",
	"接入点已改变, 原密钥不再沿用, 请重新填写密钥": "The endpoint changed, so the stored key was not reused. Please enter the key again.",
	"该会话的模型配置不可用":              "The model profile for this session is unavailable",
	"该会话上一轮还在处理中, 请等它结束后再继续":   "This session is still processing the previous turn; wait for it to finish",
	"等上一轮结束后再继续; 刷新页面可看到最新进展":  "Wait for the current turn to finish, then continue. Refresh to see the latest progress.",
	"请分批执行": "Please run in batches",
	"请在 config.toml 里设置 server.allow_ui_config=true 后重启": "Set server.allow_ui_config=true in config.toml and restart",
	"请在 config.toml 里设置 server.inspect.enabled=true 后重启": "Set server.inspect.enabled=true in config.toml and restart",
	"请在 config.toml 里设置 server.kb.enabled=true 后重启":      "Set server.kb.enabled=true in config.toml and restart",
	"请提供 documents 数组或 name+content":                     "Provide a documents array, or name and content",
	"请提供密码或私钥":                                           "Provide a password or a private key",
	"请提供查询词 q":                                           "Provide a query (q)",
	"请提供登录用户名":                                           "Provide the login user",
	"请查看服务日志":                                            "See the service log",
	"请输入问题":                                              "Enter a question",
	"读取失败":                                               "Read failed",
	"路径无效: %w":                                           "Invalid path: %w",
	"路径超出导入目录范围":                                         "Path escapes the import directory",
	"需要提供 answers (逐项回答) 或 text (补充说明)":                  "Provide answers (item by item) or text (free-form details)",
}

// 带占位符的响应文案 (语序与中文不同, 整句翻译)。
var serverTemplates = map[string]string{
	"文档不存在: {0}":     "Document not found: {0}",
	"会话不存在: {0}":     "Session not found: {0}",
	"会话不存在或已过期: {0}": "Session not found or expired: {0}",
	"巡检记录不存在: {0}":   "Inspection record not found: {0}",
	"提供的地址都不合法: {0}": "None of the provided addresses are valid: {0}",
	"未知路径: {0}":      "Unknown path: {0}",
	"生成 PDF 失败: {0}": "Failed to generate the PDF: {0}",
	"请至少提供一台主机地址":    "Provide at least one host address",
}

// 前缀类文案: 后面跟着动态值 (会话 ID、引擎名…), 只能翻前缀。
var serverPrefixes = map[string]string{
	"不支持的引擎: ":    "Unsupported engine: ",
	"会话不存在: ":     "Session not found: ",
	"会话不存在或已过期: ": "Session not found or expired: ",
	"无法解析: ":      "Cannot parse: ",
}

func init() {
	Register(serverEnglish)
	Register(serverTemplates)
	Register(serverPrefixes)
}
