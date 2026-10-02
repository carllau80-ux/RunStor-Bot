package i18n

// english 是全部中译英条目 (各文件在 init 里合并进来)。
//
// 分文件是为了让人能按模块校对: 界面文案、巡检报告、知识库、模型设置各一份。
var english = map[string]string{}

// Register 供各文件把词条并入总表 (init 里调用)。
func Register(entries map[string]string) {
	for key, value := range entries {
		english[key] = value
	}
}
