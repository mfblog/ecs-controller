# ECS 控制台

一个简单的阿里云 ECS 管理面板，可以查看实例状态和流量，并在流量达到阈值时自动关机，避免产生额外费用。

> 阿里云流量数据可能存在延迟，自动关机不能保证绝对不会产生费用。

## 部署

准备一台安装了 Docker 的公网 Linux 服务器。如果使用域名，请先将域名解析到服务器 IP。

在服务器上运行：

```bash
curl -fsSL https://raw.githubusercontent.com/Kori1c/ecs-controller/main/install.sh \
  | env ECS_COOKIE_SECURE=1 bash
```

安装脚本会自动下载镜像并启动服务，不会在服务器上编译镜像。服务默认只监听服务器本机的 `43211` 端口，请通过你现有的 HTTPS 访问方式打开面板，不要直接开放这个端口。

公网使用 Passkey 时必须使用 HTTPS。

### 本地测试

在自己的电脑上测试时，可以使用 Docker Desktop 或 Colima，安装后访问 `http://127.0.0.1:43211`。

## 第一次使用

1. 安装完成后，脚本会显示一个初始化 token。
2. 打开网页，输入 token 并设置管理员密码，密码至少 6 位。
3. 进入“账号管理”，添加阿里云账号。
4. 在“系统设置”中设置流量阈值、自动关机和通知方式。

如果忘记了初始化 token，可以查看日志：

```bash
docker logs ecs-controller 2>&1 | grep '初始化 token'
```

## 阿里云权限

请在阿里云 RAM 中创建一个用于本项目的用户，开启“OpenAPI 调用访问”，然后把 AccessKey ID 和 AccessKey Secret 填入账号管理。

项目会根据功能使用以下权限：

- **实例管理**：ECS
- **实例流量**：CMS
- **账号流量**：CDT
- **创建实例和 EIP**：VPC、ECS
- **消费情况**：账单服务

如果出现“CMS 不可用”或“CDT 不可用”，通常是对应的 RAM 权限没有开启。只查看和管理已有实例时，不需要开通创建实例、EIP 和账单权限。

## 主要功能

- 查看 ECS 状态、规格、IP 和流量。
- 查看 CMS 实例流量和 CDT 账号流量。
- CMS 或 CDT 任一流量达到阈值时自动关机。
- 定时开关机和抢占式实例自动恢复。
- 创建、启动、停止和释放 ECS。
- Telegram、邮件和 Webhook 通知。
- Passkey 免密码登录。
- 自定义控制台名称。
- 在系统设置中创建加密备份，并恢复账号、设置、历史数据和 Passkey。

### 数据备份

进入“系统设置 → 全局设置”，输入备份密码后下载备份文件。恢复时选择备份文件并输入同一个密码。

备份密码不会保存在系统中，请单独妥善保存。恢复会覆盖当前账号和设置，完成后需要使用备份中的管理员凭据重新登录。

## 在线更新

进入“关于 → 版本中心”，点击检查更新即可。

只有 GitHub 上的 Docker 镜像已经准备好时才会更新，部署机器不会构建镜像。更新不会删除账号、配置和流量数据。

如果旧版本没有在线更新功能，重新运行一次安装命令即可。

## 安全提醒

- 不要把 AccessKey、AccessKey Secret 或初始化 token 发给别人。
- 公网访问请使用 HTTPS，不要直接暴露 `43211` 端口。
- 建议使用 RAM 子账号，不要直接使用阿里云主账号密钥。

## Passkey

登录后进入“系统设置 → 全局设置”，点击“设置 Passkey”。以后可以使用指纹、面容、系统 PIN 或安全密钥登录。

公网使用 Passkey 时必须配置 HTTPS；本地测试可以使用 `http://localhost`。

## 请喝咖啡

如果这个工具对你有帮助，欢迎支持项目：

![项目赞赏码](./static/donation-code.jpg)

- USDT-TRC20：`TMBmXngHcKtNo9nLxTCJcGTfNutsbb9rp3`
- USDT-Polygon / BSC / ERC20 / ArbitrumOne：`0x846ad38ed7159aee70b0ebd8ec39ad2d7b32835b`

## 相关链接

- 项目地址：https://github.com/Kori1c/ecs-controller
- 问题反馈：https://github.com/Kori1c/ecs-controller/issues
- 许可证：[MIT](./LICENSE)
