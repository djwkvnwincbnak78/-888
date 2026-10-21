# Nexus Console (Vue 3 + Vite + TypeScript)

商用级控制台：Dashboard 数据大屏、渠道管理、令牌管理、系统设置。

## 技术栈
Vue 3.5 · Vite 5 · TypeScript · Pinia · Vue Router 4 · Element Plus 2.8 ·
TailwindCSS 3.4 · ECharts 5.5 · Axios

## 本地开发
```bash
npm install
npm run dev        # http://localhost:5173/console/
```
Vite 已将 `/admin/api` 与 `/v1` 代理到 `http://127.0.0.1:8080`。

## 构建
```bash
npm ci             # 或 npm install
npm run build      # 产物 dist/，base = /console/
npm run type-check # vue-tsc
```

## 部署（Nginx）
```nginx
location /console/ {
    alias /opt/nexus-console/;
    try_files $uri $uri/ /console/index.html;
}
```
API 由同源反代提供：
```nginx
location /admin/api/ { proxy_pass http://127.0.0.1:8080; proxy_set_header Host $host; }
location /v1/        { proxy_pass http://127.0.0.1:8080; proxy_set_header Host $host;
                       proxy_buffering off; proxy_read_timeout 600s; }
```

## 目录
```
src/
  api/         client.ts（axios 拦截器） + nexus.ts（类型化接口）
  layout/      Sidebar / Navbar / 响应式壳
  router/      4 条路由 + 登录守卫
  store/       Pinia auth（token 持久化）
  styles/      theme.css（深空黑 × 电光紫 + Element Plus 覆盖）
  views/       login / dashboard / channels / tokens / settings
```

## 安全
- JWT 存 `localStorage.nexus_jwt`，401 自动清理并跳登录。
- 上游密钥只在服务端解密，前端永远拿不到明文。
- 令牌明文只在创建响应中出现一次。
