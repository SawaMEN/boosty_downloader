'use strict';
// Administrator policy is authoritative. These controls never grant privileges
// by themselves; all changes are validated by server handlers.
const capabilityLabels = {
  connectBoosty: ['Подключение Boosty','Личный аккаунт и отключение'],
  browsePosts: ['Просмотр публикаций','Лента, текст и комментарии'],
  streamMedia: ['Видео и скачивание на ПК','Потоковое воспроизведение'],
  createArchives: ['Создание архивов','Сохранение и обновление постов'],
  saveComments: ['Архивирование комментариев','Включая ответы'],
  saveFiles: ['Архивирование файлов','Документы и вложения'],
  saveVideos: ['Архивирование видео','Видео в медиатеке'],
  saveAudio: ['Архивирование аудио','Аудиозаписи'],
  importAuthors: ['Массовый импорт авторов','Выбор из подписок Boosty'],
  autoArchive: ['Автоматическая архивация','Новые посты и обновления по расписанию'],
  shareDirect: ['Поделиться с участником','Выдать персональный доступ'],
  sharePublic: ['Публичный внутри сервиса архив','Открыть всем зарегистрированным'],
  browseShared: ['Общие архивы','Поиск, просмотр и скачивание'],
  deleteArchives: ['Удаление архивов','Удаление поста или всех копий автора']
};
let currentPolicy=null;
let adminUserRows=[];
let editedOverrides={};

function permissionValue(k){return state.user?.admin || Boolean(state.user?.permissions?.[k])}
function updatePermissionUI(){
  if(!state.user)return;
  const can=permissionValue;
  for(const [page,key] of [['browse','browsePosts'],['shared','browseShared']]){
    const navButton=document.querySelector(`[data-page="${page}"]`);
    if(navButton)navButton.hidden=!can(key);
  }
  const pick=can('importAuthors');
  $('pickSubscriptions').hidden=!pick;
  $('pickSubscriptionsFromBoosty').hidden=!pick;
  $('startArchive').hidden=!can('createArchives');
  $('connectCookie').disabled=!can('connectBoosty');
  $('connectTokens').disabled=!can('connectBoosty');
  $('deleteAuthorArchives').hidden=!can('deleteArchives');
  const keys={Comments:'saveComments',Files:'saveFiles',Video:'saveVideos',Audio:'saveAudio',Sync:'autoArchive'};
  for(const [suffix,key] of Object.entries(keys)){
    for(const prefix of ['option','archive']){
      const checkbox=$(prefix+suffix);
      checkbox.disabled=!can(key);
      if(!can(key)) checkbox.checked=false;
    }
  }
  $('adminControlPanel').hidden=!state.user.admin;
  $('myRights').replaceChildren();
  if(!state.user.admin){
    for(const [key,entry] of Object.entries(capabilityLabels)){
      const item=el('div','rights-item');
      item.append(text('strong',entry[0]));
      item.append(text('span',can(key)?'Разрешено':'Запрещено',can(key)?'rights-on':'rights-off'));
      $('myRights').append(item);
    }
  }
  $('myRightsCard').hidden=Boolean(state.user.admin);
}
function flagControl(key,value,change){
  const line=el('label','policy-flag check');
  const checkbox=el('input');checkbox.type='checkbox';checkbox.checked=value;
  checkbox.onchange=()=>change(checkbox.checked);
  const detail=el('span');detail.append(text('strong',capabilityLabels[key][0]));detail.append(text('small',capabilityLabels[key][1]));
  line.append(checkbox,detail);return line;
}
async function loadAdminPanel(){
  if(!state.user?.admin)return;
  try{
    const [p,users]=await Promise.all([api('/api/admin/policy'),api('/api/admin/users')]);
    currentPolicy=p;adminUserRows=users;
    $('adminRegistration').checked=p.registrationOpen;
    $('adminSyncHours').value=String(p.syncIntervalHours);
    $('adminMaxScan').value=String(p.maxScanPosts);
    $('adminMaxImport').value=String(p.maxImportPosts);
    $('adminCompressionLevel').value=String(p.compressionLevel);
    $('adminDeduplicateMedia').checked=p.deduplicateMedia;
    $('adminDefaults').replaceChildren();
    for(const key of Object.keys(capabilityLabels)){
      $('adminDefaults').append(flagControl(key,p.defaults[key],checked=>{currentPolicy.defaults[key]=checked}));
    }
    const selected=$('adminUserPicker').value;
    $('adminUserPicker').replaceChildren();
    for(const u of users){if(u.admin)continue;const opt=el('option');opt.value=u.id;opt.textContent=u.username;$('adminUserPicker').append(opt)}
    if(users.some(u=>u.id===selected&&!u.admin))$('adminUserPicker').value=selected;
    selectPolicyUser();
  }catch(error){toast(error.message,true)}
}
function selectPolicyUser(){
  const id=$('adminUserPicker').value;
  const user=adminUserRows.find(item=>item.id===id);
  editedOverrides={...user?.overrides};
  const list=$('adminUserFeatures');list.replaceChildren();
  if(!user){list.append(text('p','Пользователей пока нет','muted'));$('saveUserRights').disabled=true;return}
  $('saveUserRights').disabled=false;
  for(const key of Object.keys(capabilityLabels)){
    const row=el('label','user-permission');
    row.append(text('span',capabilityLabels[key][0]));
    const select=el('select');select.dataset.feature=key;
    for(const [value,name] of [['default','По умолчанию'],['true','Разрешить'],['false','Запретить']]){
      const opt=el('option');opt.value=value;opt.textContent=name;select.append(opt);
    }
    select.value=Object.prototype.hasOwnProperty.call(editedOverrides,key)?String(editedOverrides[key]):'default';
    select.onchange=()=>{if(select.value==='default')delete editedOverrides[key];else editedOverrides[key]=select.value==='true'};
    row.append(select);list.append(row);
  }
}
async function savePolicy(){
  if(!currentPolicy)return;
  const button=$('adminSavePolicy');button.disabled=true;
  const data={defaults:currentPolicy.defaults,registrationOpen:$('adminRegistration').checked,
    syncIntervalHours:Number($('adminSyncHours').value),maxScanPosts:Number($('adminMaxScan').value),maxImportPosts:Number($('adminMaxImport').value),
    compressionLevel:Number($('adminCompressionLevel').value),deduplicateMedia:$('adminDeduplicateMedia').checked};
  try{
    currentPolicy=await api('/api/admin/policy',{method:'PUT',body:data});
    state.user=await api('/api/account');
    updatePermissionUI();toast('Политика сервера сохранена. Новое расписание уже действует.');
    await loadAdminPanel();
  }catch(e){toast(e.message,true)}finally{button.disabled=false}
}
async function saveUserRights(){
  const id=$('adminUserPicker').value;if(!id)return;
  const button=$('saveUserRights');button.disabled=true;
  try{await api(`/api/admin/users/${encodeURIComponent(id)}/permissions`,{method:'PUT',body:{overrides:editedOverrides}});toast('Индивидуальные разрешения сохранены');await loadAdminPanel();}
  catch(error){toast(error.message,true)}finally{button.disabled=false}
}
function adminEvents(){
  $('adminSavePolicy').onclick=savePolicy;
  $('adminReloadPolicy').onclick=loadAdminPanel;
  $('adminUserPicker').onchange=selectPolicyUser;
  $('saveUserRights').onclick=saveUserRights;
}
