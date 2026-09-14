import { existsSync, readdirSync, rmdirSync, unlinkSync } from 'node:fs'
import { join } from 'node:path'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 面板的服务端（账号管理、密钥读写、日志统计）在网关进程里，即 /__admin/*。
// 开发时由这里把 /api 与 /__admin 一并反代到网关，于是前端仍享受 HMR，
// 而两种部署形态下前端代码完全一致（都只用相对路径）。
//
// 为什么必须反代而不是让浏览器直连：网关不发 CORS 头，跨源请求会被浏览器拦下。
// 同源 → 服务端转发 → 不涉及 CORS。
const GATEWAY = process.env.GATEWAY_URL ?? 'http://127.0.0.1:7863'

// 产物直接落进 internal/web/，由该包的 //go:embed all:dist 编进 exe。
// 放这里而不是 frontend/dist 再拷一份：go:embed 不能引用包目录之外，
// 单一产物目录省掉了构建脚本里的复制步骤。
const OUT_DIR = '../internal/web/dist'

const proxy = {
  // /api/* → 网关 /*（去掉 /api 前缀，与网关实际路由对齐）。
  '/api': {
    target: GATEWAY,
    changeOrigin: true,
    rewrite: (p: string) => p.replace(/^\/api/, ''),
  },
  // /__admin/* → 网关同名路径（网关内部会校验请求来自本机）。
  '/__admin': {
    target: GATEWAY,
    changeOrigin: true,
  },
}

/**
 * 递归删除目录（自己走一遍，不用 fs.rmSync）。
 *
 * 为什么不用 fs.rmSync：outDir 落在项目根之外时它实测什么都不删，也不报错
 * （vite 8.3.0 下解析出的 build.emptyOutDir 明明是 true，目录却原封不动；
 * 根因是 rmSync 内部重试 EBUSY 后把最终错误吞掉了）。后果不只是留垃圾 ——
 * 带内容哈希的旧 JS 会一直堆着，并被 go:embed 一并编进二进制，越滚越大，而两边都不报错。
 *
 * unlinkSync + rmdirSync 的组合没有这个问题，且不吞错。
 */
function removeDir(p: string) {
  if (!existsSync(p)) return
  for (const e of readdirSync(p, { withFileTypes: true })) {
    const child = join(p, e.name)
    if (e.isDirectory()) removeDir(child)
    else unlinkSync(child)
  }
  rmdirSync(p)
}

/** 构建前清空产物目录，避免陈的哈希资源堆积。 */
function cleanOutDir() {
  return {
    name: 'clean-out-dir',
    buildStart() {
      removeDir(OUT_DIR)
    },
  }
}

export default defineConfig({
  plugins: [react(), cleanOutDir()],
  build: {
    outDir: OUT_DIR,
  },
  server: {
    // 端口固定：每次重启换端口会让书签与已配好的客户端失效。
    port: 7864,
    strictPort: true,
    proxy,
  },
  preview: {
    port: 7864,
    strictPort: true,
    proxy,
  },
})
