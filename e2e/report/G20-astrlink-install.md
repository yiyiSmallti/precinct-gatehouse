# G20 AstrLink NER 一键安装与配置

结果：**通过** · 运行于 2026-11-25 04:52:50 PST · darwin/arm64

验证管理台 /admin/api/redaction/astrlink 状态查询、一键下载插件/ORT/模型并自动写入 config.yaml 的完整端到端流程。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| 初始状态未安装且未配置 | ✅ | status=map[binary_path:/Volumes/OpenClawSSD/migrated/cache/a1/tmp/TestG20AstrlinkInstall3473675964/001/plugins/astrlink/bin/astrlink-detector binary_ready:false configured:false error: installing:false max_action:audit message: model_dir:/Volumes/OpenClawSSD/migrated/cache/a1/tmp/TestG20AstrlinkInstall3473675964/001/plugins/astrlink/models/astrlink-int8 model_ready:false ort_path:/Volumes/OpenClawSSD/migrated/cache/a1/tmp/TestG20AstrlinkInstall3473675964/001/plugins/astrlink/lib/libonnxruntime.1.27.0.dylib ort_ready:false platform:darwin/arm64 restart_required:false running:false step: supported:true] |
| 一键安装与配置完成 | ✅ | step=done err= |
| config.yaml 已写入 plugins 与 redaction.sources.astrlink | ✅ | config.yaml updated |
