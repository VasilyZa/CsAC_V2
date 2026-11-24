# 空间

[返回 API 手册](../api.md) · [上传图片](media.md#申请和上传) · [事件同步](events.md)

空间命令使用 `member`；空间动态由本人及当前互未屏蔽的好友查看；评论及配图还依据评论作者和根动态的当前可见性过滤；页面不把曾经可见的内容视为永久授权，好友关系变化后刷新对应时间线

## 读取时间线与评论

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `space.feed` | — | `{author_id?,before_id?,limit?,include_total?}` | `{posts: Post[],next_before_id?,total?}` |
| `space.replies` | — | `{post_id,after_id?,limit?}`，`post_id` 为根动态 ID | `{replies: Item[],next_after_id?}` |

两者默认 `limit: 20`，范围 1–50；下列游标都用 JSON 整数，省略游标读取第一页，响应缺少下一页字段时结束

- `space.feed` 按根动态 `id` 降序，`next_before_id` 传给 `before_id`；`author_id` 省略或为 `0` 返回自己的动态与好友动态，正整数只查该作者；无法访问该作者时返回 `not_found`
- `include_total: true` 返回 `total`，是符合作者和可见性条件的根动态总量，不受此页 `before_id` 限制；默认不计算
- 根动态附带最早的至多 3 条可见评论；`next_reply_after_id` 存在时可用 `space.replies {post_id,after_id}` 接着预览尾部；评论按 `id` 升序，继续翻页用 `next_after_id → after_id`

### 动态与评论对象

| `Item` 字段 | 类型与语义 |
| --- | --- |
| `id`、`author_id`、`nickname` | 条目整数 ID、作者 UID、昵称 |
| `avatar_id?` | 作者头像 UUID |
| `parent_id?` | 评论所属根动态 ID；根动态不带这个字段 |
| `reply_to_id?` | 楼中楼的目标评论 ID；目标删除或当前用户看不到时省略 |
| `content` | Markdown 源文或纯文本，作为普通用户内容渲染 |
| `media_ids` | 媒体 UUID 数组，按作者传入顺序，空数组表示没有图片 |
| `created_at` | 创建时间 |
| `likes_count`、`liked` | 当前仍符合可见性的点赞数，以及自己是否点赞 |

`Post` 包含所有 `Item` 字段，另有 `replies: Item[]` 和可选的 `next_reply_after_id`；同一动态下的所有评论都直接归属于根动态，用 `reply_to_id` 表达「回复哪条评论」，不另外嵌套一层请求和分页

## 发布、回复、点赞与删除

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `space.create` | 是 | `{content?,media_ids?}` | `{id}`，新根动态 ID |
| `space.reply` | 是 | `{post_id,reply_to_id?,content?,media_ids?}` | `{id}`，新评论 ID |
| `space.like.toggle` | 是 | `{post_id}`，可为可见的动态或评论 ID | `{liked,likes_count}` |
| `space.delete` | 是 | `{post_id}`，自己的动态或评论 ID | `{id}`，已删除条目 ID |

文本最多 4,000 UTF-8 字节，非空文本保持 Markdown 的缩进、换行和原始空白；至少填非空白正文或一张图片；每条动态或评论最多 9 张图片，`media_ids` 不重复，使用自己的已上传图片且 `purpose: "space"`；只有图片时服务端保存默认文字 `分享图片`

```json
{"id":"post-1","op":"space.create","key":"space-post-3ba7f9ab","data":{"content":"## 更新记录\n\n- 支持 **Markdown**\n- 保留段落和缩进"}}
```

```json
{"id":"reply-2","op":"space.reply","key":"space-reply-518a03b8","data":{"post_id":308,"reply_to_id":311,"content":"谢谢分享，已试用"}}
```

楼中楼的 `reply_to_id` 必须属于同一根动态，且当前可见；删掉目标评论会取消其他评论对它的引用，其他评论保留；删根动态会同时删除其评论与点赞；作者可以撤下自己的评论，即使该动态已不再对自己可见

`space.like.toggle` 根据服务端当前状态反转点赞；一次点击对应一个 `key`；重试同一次点击时沿用原键，使用新键表示再切换一次；收到 `space.changed` 时刷新受影响的可视范围；该推送没有动态 ID
