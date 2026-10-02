# 额度持久化容量核查

本记录针对仓库固定基线 `48d7827458b07a20c3a679b4448ca5a80321db72`，使用 Go 1.27.1、GORM 1.30.0、MySQL/PostgreSQL/SQLite 驱动 1.6.0，在 linux/amd64 上执行。项目发布配置支持 amd64/arm64。本次没有部署数据库连接，未检查旧库实际类型、迁移历史和当前值域。

| 字段 | Go 类型 | MySQL 生成类型 | PostgreSQL 生成类型 | SQLite 实际新建列类型 |
|---|---|---|---|---|
| users.quota、used_quota、request_count、aff_quota、aff_history | int（64 位构建） | BIGINT | BIGINT | INTEGER |
| tokens.remain_quota、used_quota | int（64 位构建） | BIGINT | BIGINT | INTEGER |
| channels.used_quota | int64 | BIGINT | BIGINT | INTEGER |
| orders.quota | int（64 位构建） | BIGINT | BIGINT | INTEGER |

MySQL/PostgreSQL 结果来自当前 GORM dialect 的 FullDataTypeOf，使用本地连接做离线类型映射，没有在真实服务器执行 CREATE/ALTER。SQLite 结果通过临时文件数据库 AutoMigrate、ColumnTypes 及读写校验获得。SQLite INTEGER 支持有符号 64 位值；测试覆盖 2^31、2^40 和 MaxInt64−8 的存储、读取及安全的 +4 增量。

不能从 `gorm:"type:int"` 直接推断数据库为 32 位：当前 GORM 根据 Go 字段 Size=64 转成 BIGINT。源码未发现针对这些额度列的显式容量迁移，但旧部署可能使用历史 Schema、不同架构/驱动或手工修改。当前证据没有触发容量扩列条件，因此不添加推测性迁移。这些持久化测试不证明所有业务金额计算在 MaxInt64 附近都安全；真实充值、消费和订单升级仍须采用既有校验和回归。

## 部署数据库只读核查

在目标数据库的只读连接中执行以下查询，并记录版本、列类型、值域和迁移历史；不要在生产试跑 ALTER。

MySQL（将 DATABASE() 对应的库确认是目标部署）：

```sql
SELECT VERSION();
SELECT table_name, column_name, column_type, is_nullable
FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND ((table_name = 'users' AND column_name IN ('quota','used_quota','request_count','aff_quota','aff_history'))
    OR (table_name = 'tokens' AND column_name IN ('remain_quota','used_quota'))
    OR (table_name = 'channels' AND column_name = 'used_quota')
    OR (table_name = 'orders' AND column_name = 'quota'))
ORDER BY table_name, column_name;
```

PostgreSQL（确认 current_schema()，非默认 schema 时使用实际 schema）：

```sql
SELECT version(), current_schema();
SELECT table_schema, table_name, column_name, data_type, udt_name, is_nullable
FROM information_schema.columns
WHERE table_schema = current_schema()
  AND ((table_name = 'users' AND column_name IN ('quota','used_quota','request_count','aff_quota','aff_history'))
    OR (table_name = 'tokens' AND column_name IN ('remain_quota','used_quota'))
    OR (table_name = 'channels' AND column_name = 'used_quota')
    OR (table_name = 'orders' AND column_name = 'quota'))
ORDER BY table_name, column_name;
```

SQLite 使用只读模式打开目标文件，再执行：

```sql
SELECT sqlite_version();
PRAGMA table_info(users);
PRAGMA table_info(tokens);
PRAGMA table_info(channels);
PRAGMA table_info(orders);
```

三类数据库均可执行值域查询（大表可能全表扫描，应在维护窗口或副本评估）：

```sql
SELECT MIN(quota), MAX(quota), MIN(used_quota), MAX(used_quota),
       MIN(request_count), MAX(request_count), MIN(aff_quota), MAX(aff_quota),
       MIN(aff_history), MAX(aff_history) FROM users;
SELECT MIN(remain_quota), MAX(remain_quota), MIN(used_quota), MAX(used_quota) FROM tokens;
SELECT MIN(used_quota), MAX(used_quota) FROM channels;
SELECT MIN(quota), MAX(quota) FROM orders;
```

确认迁移表存在后读取 `SELECT id FROM migrations ORDER BY id;`，对照部署提交的 `model/migrate.go`。若实际列为 32 位或容量不足，再统一评估余额、累计、令牌及订单列和应用数值校验，提供备份、迁移及业务路径验证。扩容后已写入大值时不能无条件缩列回滚。

复现本地回归：`go test ./model -run TestQuotaPersistence -count=1 -v`。这两项测试只验证 64 位环境，其他环境调用 Skip；未执行 32 位全仓库交叉构建，也未验证其部署支持面。
