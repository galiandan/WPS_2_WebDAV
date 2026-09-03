# 请求记录模板

复制本模板到本机实验笔记中使用。提交到仓库前，确认没有 Cookie、Token、签名值、真实个人信息或完整文件内容。

```text
experiment:
observed_at:
ui_action:
account_scope: own account / own test folder

request:
  method:
  origin_host:
  path:
  query_parameter_names:
  header_names:
  body_content_type:
  body_shape:

response:
  status:
  redirect_chain:
  content_type:
  body_shape:
  pagination:
  error_shape:

file_or_folder_observation:
  visible_name:
  kind: file / folder / unknown
  visible_size:
  parent_ui_name:
  candidate_id_field_names:

replay_status: not attempted / local prototype succeeded / failed
evidence: observed / reproduced / inferred / unknown
related_experiments:
notes:
```
