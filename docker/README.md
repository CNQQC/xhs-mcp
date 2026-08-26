# Docker 使用说明

## 0. 重点注意

写在最前面。

- 启动后，会产生一个 `images/` 目录，用于存储发布的图片。它会挂载到 Docker 容器里面。
  如果要使用本地图片发布的话，请确保图片拷贝到 `./images/` 目录下，并且让 MCP 在发布的时候，指定文件夹为：`/app/images`，否则一定失败。
- Docker 镜像内置浏览器，并在构建阶段预下载。请挂载 `./data:/app/data`，用于持久化 cookies 和运行数据目录。

## 1. 获取 Docker 镜像

### 1.1 从 Docker Hub 拉取（推荐）

我们提供了预构建的 Docker 镜像，可以直接从 Docker Hub 拉取使用：

```bash
# 拉取最新镜像
docker pull xpzouying/xiaohongshu-mcp
```

Docker Hub 地址：[https://hub.docker.com/r/xpzouying/xiaohongshu-mcp](https://hub.docker.com/r/xpzouying/xiaohongshu-mcp)

### 1.2 从阿里云镜像源拉取（国内用户推荐）

国内用户可以使用阿里云容器镜像服务，拉取速度更快：

```bash
# 拉取最新镜像
docker pull crpi-hocnvtkomt7w9v8t.cn-beijing.personal.cr.aliyuncs.com/xpzouying/xiaohongshu-mcp
```

### 1.3 自己构建镜像（要用本仓库的改动，走这条）

在项目根目录（有 Dockerfile 的那一级）运行：

```bash
docker build --build-arg VERSION=$(git describe --tags --always --dirty) -t xhs-mcp .
```

`VERSION` 经 ldflags 编进二进制，`/health` 才报得出跑的是哪个 commit；
漏了它就一直是 `dev`，线上出问题时只能靠猜。

走 compose 的话不必手动构建——`docker compose up -d --build` 会自己来（见下一节）。

<img width="2576" height="874" alt="image" src="https://github.com/user-attachments/assets/fe7e87f1-623f-409f-8b54-e11d380fc7b8" />

## 2. 手动 Docker Compose

本仓库的 `docker-compose.yml` 带了 `build:`，`docker compose up -d` 会直接从源码构建，
不必先手动 `docker build`；改了源码用 `docker compose up -d --build` 重建。
两个该配的环境变量见 [1.3](#13-自己构建镜像要用本仓库的改动走这条)（`VERSION`）和
[第 6 节](#6-配置验证网页的公网地址xhs_public_base_url)（`XHS_PUBLIC_BASE_URL`）。

> **国内用户提示**：如需使用阿里云镜像源，请修改 `docker-compose.yml` 文件，注释掉 Docker Hub 镜像行，取消阿里云镜像行的注释：
> ```yaml
> # image: xpzouying/xiaohongshu-mcp
> image: crpi-hocnvtkomt7w9v8t.cn-beijing.personal.cr.aliyuncs.com/xpzouying/xiaohongshu-mcp
> ```

```bash
# 注意：在 docker-compose.yml 文件的同一个目录，或者手动指定 docker-compose.yml。

# --- 启动 docker 容器 ---
# 启动 docker-compose
docker compose up -d

# 查看日志
docker logs -f xiaohongshu-mcp

# 或者
docker compose logs -f
```

查看日志，下面表示成功启动。

<img width="1012" height="98" alt="image" src="https://github.com/user-attachments/assets/c374f112-a5b5-4cf6-bd9f-080252079b10" />


```bash
# 停止 docker-compose
docker compose stop

# 查看实时日志
docker logs -f xiaohongshu-mcp

# 进入容器
docker exec -it xiaohongshu-mcp bash

# 手动更新容器
docker compose pull && docker compose up -d
```

## 3. 使用 MCP-Inspector 进行连接

**注意 IP 换成你自己的 IP**

<img width="2606" height="1164" alt="image" src="https://github.com/user-attachments/assets/495916ad-0643-491d-ae3c-14cbf431c16f" />

对应的 Docker 日志一切正常。

<img width="1662" height="458" alt="image" src="https://github.com/user-attachments/assets/309c2dab-51c4-4502-a41b-cdd4a3dd57ac" />

## 4. 配置代理（可选）

如果需要通过代理访问小红书，可以通过 `XHS_PROXY` 环境变量配置。

### 使用 docker run

```bash
docker run -e XHS_PROXY=http://user:pass@proxy:port xpzouying/xiaohongshu-mcp
```

### 使用 docker-compose

在 `docker-compose.yml` 的 `environment` 中添加 `XHS_PROXY`：

```yaml
environment:
  - COOKIES_PATH=/app/data/cookies.json
  - HOME=/app/data/home
  - XDG_CONFIG_HOME=/app/data/config
  - XHS_PROXY=http://user:pass@proxy:port
```

支持 HTTP/HTTPS/SOCKS5 代理。日志中会自动隐藏代理的认证信息，输出示例：

```
Using proxy: http://***:***@proxy:port
```

## 5. 配置访问鉴权（可选）

不设置或设置为空时，鉴权默认关闭。生产环境建议通过 `AUTH_TOKEN` 环境变量配置访问令牌。

### 使用 docker run

```bash
docker run -e AUTH_TOKEN=your-secret-token -p 18060:18060 xpzouying/xiaohongshu-mcp
```

### 使用 docker compose

```bash
AUTH_TOKEN=your-secret-token docker compose up -d
```

Compose 通过 `${AUTH_TOKEN:-}` 读取宿主环境变量。启用鉴权后，所有 MCP 客户端请求都必须带上自定义请求头：`Authorization: Bearer <token>`。

## 6. 配置验证网页的公网地址（`XHS_PUBLIC_BASE_URL`）

账号被小红书安全验证拦住时，`get_verification_qrcode` 会发一条一次性链接
`/verify/<token>`，用手机浏览器打开就能看到大二维码、扫完自动显示结果——
这是人不在电脑前也能解封的主路径。

服务端猜不到自己的公网地址，也**故意不拿请求的 `Host` 头去猜**：反代后面猜错了，
用户点开是坏链接，只会以为功能坏了。所以不配这个变量就只给相对路径，等于没有可点的链接。

### 使用 docker compose

```bash
XHS_PUBLIC_BASE_URL=https://api.example.com/xhs docker compose up -d
```

反代带路径前缀时要**连前缀一起写**（上面那个 `/xhs` 就是）。
`/verify/*` 这两条路由不走 `AUTH_TOKEN` 鉴权（手机浏览器发不了 `Authorization` 头），
路径里那个 32 字节随机 token 就是凭证：15 分钟有效、不写进访问日志、页面不引任何外部资源。

## 7. 扫码登录

1. **重要**，一定要先把 App 提前打开，准备扫码登录。
2. 尽快扫码，有可能二维码会过期。

打开 MCP-Inspector 获取二维码和进行扫码。

<img width="2632" height="1468" alt="image" src="https://github.com/user-attachments/assets/543a5427-50e3-4970-b942-5d05d69596f4" />

<img width="2624" height="1222" alt="image" src="https://github.com/user-attachments/assets/4f38ca81-1014-4874-ab4d-baf02b750b55" />

扫码成功后，再次扫码后，就会提示已经完成登录了。

<img width="2614" height="994" alt="image" src="https://github.com/user-attachments/assets/5356914a-3241-4bfd-b6b2-49c1cc5e3394" />
