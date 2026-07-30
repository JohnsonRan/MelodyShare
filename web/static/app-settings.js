'use strict';

// ---- settings view ----

let settingsLoaded = false;

async function loadSettings() {
  if (settingsLoaded) return;
  $('settingsView').setAttribute('aria-busy', 'true');
  try {
    const st = await api('GET', '/api/settings');
    $('setSiteName').value = st.siteName;
    $('setBaseURL').value = st.baseURL;
    const autoChunk = st.chunkSizeMB === 0;
    $('setChunkMode').value = autoChunk ? 'auto' : 'manual';
    $('setChunkSize').value = autoChunk ? 64 : st.chunkSizeMB;
    $('chunkSizeRow').hidden = autoChunk;
    $('setR2Endpoint').value = st.r2.endpoint;
    $('setR2AccessKey').value = st.r2.accessKey;
    $('setR2SecretKey').value = '';
    $('setR2SecretKey').placeholder = st.r2.secretSet ? '已设置，留空保持不变' : '未设置';
    $('setR2Bucket').value = st.r2.bucket;
    $('setPasteEnabled').checked = st.pasteEnabled;
    $('setPasteMaxKB').value = st.pasteMaxKB;
    $('setUsername').value = st.username;
    $('setNewPassword').value = '';
    $('setCurrentPassword').value = '';
    settingsLoaded = true;
  } finally {
    $('settingsView').removeAttribute('aria-busy');
  }
}

$('setChunkMode').addEventListener('change', () => {
  $('chunkSizeRow').hidden = $('setChunkMode').value !== 'manual';
});

busy($('r2TestBtn'), '测试中…', async () => {
  try {
    await api('POST', '/api/settings/r2/test', {
      r2Endpoint: $('setR2Endpoint').value,
      r2AccessKey: $('setR2AccessKey').value,
      r2SecretKey: $('setR2SecretKey').value,
      r2Bucket: $('setR2Bucket').value,
    });
    toast('R2 连接成功');
  } catch (err) {
    toast(err.message);
  }
});

busy($('settingsSave'), '保存中…', async () => {
  let chunkSizeMB = 0; // 0 = 自动
  if ($('setChunkMode').value === 'manual') {
    chunkSizeMB = parseInt($('setChunkSize').value, 10);
    if (!(chunkSizeMB >= 5 && chunkSizeMB <= 95)) { toast('手动分片大小需在 5–95 MB 之间'); return; }
  }
  const pasteMaxKB = parseInt($('setPasteMaxKB').value, 10);
  if (!(pasteMaxKB >= 1 && pasteMaxKB <= 10240)) { toast('剪切板单条上限需在 1–10240 KB 之间'); return; }
  try {
    await api('PUT', '/api/settings', {
      siteName: $('setSiteName').value,
      baseURL: $('setBaseURL').value,
      chunkSizeMB,
      pasteEnabled: $('setPasteEnabled').checked,
      pasteMaxKB,
      r2Endpoint: $('setR2Endpoint').value,
      r2AccessKey: $('setR2AccessKey').value,
      r2SecretKey: $('setR2SecretKey').value,
      r2Bucket: $('setR2Bucket').value,
    });
    if (activeUploads.size) {
      // a reload now would abort the uploads; the settings are already live
      toast('已保存并生效');
    } else {
      toast('已保存，正在刷新');
      setTimeout(() => location.reload(), 600);
    }
  } catch (err) { toast(err.message); }
});

busy($('accountSave'), '更新中…', async () => {
  if (!$('setCurrentPassword').value) {
    toast('请输入当前密码');
    $('setCurrentPassword').focus();
    return;
  }
  try {
    await api('PUT', '/api/settings/account', {
      currentPassword: $('setCurrentPassword').value,
      username: $('setUsername').value,
      newPassword: $('setNewPassword').value,
    });
    toast('账号已更新');
    $('setCurrentPassword').value = '';
    $('setNewPassword').value = '';
  } catch (err) { toast(err.message); }
});

