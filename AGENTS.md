# AGENTS.md (llm-request-tollgate)

项目级规则: 本文件优先于全局 ~/.dsh/AGENTS.md; 用户当次明确要求优先于本文件。

## 版本号

- 版本号采用语义化版本 (MAJOR.MINOR.PATCH), 定义在 `src/version.go` 的
  `var version` (当前 `0.8.1`), 由 `GET /version` 接口对外返回, 不与 git commit 绑定。
- **任何代码变更 (修复缺陷 / 新功能 / 重构 / 行为调整) 提交前, 必须同步递增版本号**:
  - 修复缺陷: PATCH +1 (0.1.0 -> 0.1.1);
  - 新功能或对外行为变化: MINOR +1 (0.1.x -> 0.2.0);
  - 破坏性变化 (接口 / 配置格式不兼容): MAJOR +1 (0.x.y -> 1.0.0)。
- 只改文档 / 注释 / 纯测试断言 (不改变产物行为) 时可保持版本号不变。
- 常规 `make build` 直接用源码里的版本号; 发布构建可用 `make VERSION=x build`
  经 -ldflags 覆盖注入。
