# AutoSeedRelay Frontend (M0)

Vue 3 + Vite + TypeScript + Element Plus + Pinia + vue-router + axios 前端脚手架。

## 启动

```bash
npm install       # 安装依赖
npm run dev       # 开发服务器 http://localhost:5173（/api 代理到 http://localhost:9020）
npm run build     # 类型检查 + 构建，产物输出到 dist/
npm run preview   # 本地预览构建产物
```

## 说明

- 登录页 `/login`：POST `/api/v2/auth/login`，成功跳 `/`。
- 首页 `/`：五区空壳（顶部状态条 / 六张统计卡 / 进行中任务 / 事件流 / 7 天趋势）。
- 登录态暂存 localStorage（`autoseedrelay_token`），后续切换为后端 cookie session。
- Element Plus 当前为全量引入（M0 不关注体积），后续可切换按需引入。
