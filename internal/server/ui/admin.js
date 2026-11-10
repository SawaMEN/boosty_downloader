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
    $('adminMinFreeGiB').value=String(p.minFreeGiB??1);
    $('adminMaxUserGiB').value=String(p.maxUserGiB??0);
    loadAdminStorage();
    const v=p.video||{};
    $('adminVideoEnabled').checked=Boolean(v.enabled);
    $('adminVideoAutomatic').checked=Boolean(v.automatic);
    $('adminVideoBackend').value=v.backend||'auto';
    $('adminVideoCodec').value=v.codec||'h264';
    $('adminVideoQuality').value=String(v.quality||26);
    $('adminVideoHeight').value=String(v.maxHeight??1080);
    $('adminVideoFallback').checked=Boolean(v.fallbackCpu);
    $('adminVideoOnlySmaller').checked=Boolean(v.onlySmaller);
    loadVideoReport(false);

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
    compressionLevel:Number($('adminCompressionLevel').value),deduplicateMedia:$('adminDeduplicateMedia').checked,
    minFreeGiB:Number($('adminMinFreeGiB').value),maxUserGiB:Number($('adminMaxUserGiB').value),
    video:{enabled:$('adminVideoEnabled').checked,automatic:$('adminVideoAutomatic').checked,backend:$('adminVideoBackend').value,
      codec:$('adminVideoCodec').value,quality:Number($('adminVideoQuality').value),maxHeight:Number($('adminVideoHeight').value),
      fallbackCpu:$('adminVideoFallback').checked,onlySmaller:$('adminVideoOnlySmaller').checked}};
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
let videoArchives=[];
function renderAdminHardware(h){
  const root=$('adminHardwareReport');root.replaceChildren();
  if(!h){root.append(text('p','Отчёт об оборудовании недоступен','muted'));return}
  const cpu=h.cpu||{};
  root.append(text('h4','Процессор и инструкции'));
  root.append(text('p',`${cpu.model||'Неизвестный CPU'} · ${cpu.architecture||'—'} · ${cpu.logicalCores||0} логических потоков`,'muted small'));
  const flags=el('div','hardware-chips');
  for(const flag of cpu.instructions||[]){flags.append(text('span',flag,'hardware-chip'))}
  if(!flags.childElementCount)flags.append(text('p','Инструкции не удалось определить','muted small'));
  root.append(flags);
  for(const note of cpu.notes||[]){root.append(text('p',note,'muted small'))}
  root.append(text('h4','Графические устройства'));
  if(!(h.gpus||[]).length){root.append(text('p','GPU не обнаружен из этого процесса. В Docker может потребоваться проброс устройства.','muted small'))}
  for(const gpu of h.gpus||[]){
    const card=el('div','hardware-item');
    card.append(text('strong',`${gpu.vendor} · ${gpu.device} ${gpu.pci?'('+gpu.pci+')':''}`));
    card.append(text('p',`${gpu.driver||'Драйвер не определён'} · ${gpu.accessible?'Доступно процессу':'Нет доступа к устройству'}`,gpu.accessible?'rights-on':'error'));
    if(gpu.issue)card.append(text('p',gpu.issue,'muted small'));
    if((gpu.profiles||[]).length)card.append(text('p','VA-API (заявлено драйвером): '+gpu.profiles.join(' · '),'muted small'));
    root.append(card);
  }
  const tools=Object.entries(h.tools||{}).map(([name,ok])=>`${name}: ${ok?'есть':'нет'}`);
  root.append(text('p',`${h.distribution||h.system} · ${h.container?'Docker/контейнер':'Обычный процесс'} · ${tools.join(' · ')}`,'muted small'));
  root.append(text('p',`Интерфейсы FFmpeg (заявлены сборкой, без проверки декодирования): ${(h.accelerators||[]).join(', ')||'не обнаружены'}`,'muted small'));
  root.append(text('h4','Поддержка видеокодеров'));
  const codecs=el('div','hardware-codecs');
  for(const c of h.codecs||[]){
    const label= c.verified?'Проверено':c.built?'Есть в FFmpeg':'Нет в FFmpeg';
    const item=el('div','hardware-codec');
    item.append(text('strong',`${c.backend.toUpperCase()} · ${c.codec.toUpperCase()}`));
    item.append(text('span',label,c.verified?'rights-on':c.built?'muted small':'error'));
    codecs.append(item);
  }
  root.append(codecs);
  const profile=h.profile||{};
  root.append(text('h4','Рекомендуемый профиль'));
  root.append(text('p',`${(profile.backend||'auto').toUpperCase()} · ${(profile.codec||'h264').toUpperCase()} · ${profile.explanation||''}`,profile.verified?'rights-on':'muted'));
  if(profile.verified){
    const apply=el('button','btn subtle');apply.type='button';apply.textContent='Подставить проверенный профиль';
    apply.onclick=()=>{
      $('adminVideoBackend').value=profile.backend;
      $('adminVideoCodec').value=profile.codec;
      $('adminVideoEnabled').checked=true;
      $('adminVideoAutomatic').checked=false;
      toast('Проверенный профиль подставлен. Для применения нажмите «Сохранить настройки сервера». Автообработка не включена.');
    };
    root.append(apply);
  }
  root.append(text('h4','Что можно установить или включить'));
  if(!(h.suggestions||[]).length)root.append(text('p','Обязательных действий не обнаружено. Для подтверждения ускорения выполните пробное кодирование.','muted small'));
  for(const suggestion of h.suggestions||[]){
    const item=el('div','hardware-item');
    item.append(text('strong',suggestion.title));
    item.append(text('p',suggestion.detail,'muted small'));
    if(suggestion.command){
      const code=el('code','hardware-command');code.textContent=suggestion.command;item.append(code);
    }
    root.append(item);
  }
}
function selectAdminVideoArchive(){
  const id=$('adminVideoArchive').value;
  const entry=videoArchives.find(a=>a.id===id);
  $('adminVideoFile').replaceChildren();
  for(const name of entry?.files||[]){const option=el('option');option.value=name;option.textContent=name;$('adminVideoFile').append(option)}
  $('adminVideoConvert').disabled=!entry || !(entry.files||[]).length;
}
async function loadVideoReport(active){
  if(!state.user?.admin)return;
  const button=$('adminVideoProbe');button.disabled=true;
  $('adminVideoDiagnostics').replaceChildren(text('p',active?'Проверка кодеков и устройств…':'Загружаем оборудование…','muted'));
  $('adminHardwareReport').replaceChildren(text('p','Анализ системы…','muted'));
  try{
    const data=await api('/api/admin/video'+(active?'?probe=1':''));
    const diag=data.diagnostics||{};
    renderAdminHardware(data.hardware);
    const root=$('adminVideoDiagnostics');root.replaceChildren();
    root.append(text('p',`FFmpeg: ${diag.ffmpeg||'не установлен'} · FFprobe: ${diag.ffprobe?'есть':'нет'} · /dev/dri: ${(diag.renderDevices||[]).join(', ')||'нет'}`,'muted small'));
    const table=el('div','video-diagnostics-list');
    for(const b of diag.backends||[]){
      const item=el('div','video-diagnostic-row');
      item.append(text('strong',`${b.name.toUpperCase()} · ${b.encoder}`));
      item.append(text('span',`${b.available?'Готов':b.reason||'Требуется проверка'}${b.device?' · '+b.device:''}`,b.available?'rights-on':'muted small'));
      table.append(item);
    }
    root.append(table);
    const previous=$('adminVideoArchive').value;
    videoArchives=data.archives||[];
    $('adminVideoArchive').replaceChildren();
    for(const entry of videoArchives){const o=el('option');o.value=entry.id;o.textContent=`${entry.title||entry.author} · ${entry.owner} · ${entry.files.length} видео`;$('adminVideoArchive').append(o)}
    if(videoArchives.some(v=>v.id===previous))$('adminVideoArchive').value=previous;
    selectAdminVideoArchive();
    const task=$('adminVideoTasks');task.replaceChildren();
    const jobs=Object.entries(data.jobs||{}).sort((a,b)=>new Date(b[1].updatedAt)-new Date(a[1].updatedAt)).slice(0,12);
    if(jobs.length)task.append(text('h4','Последние задачи кодирования'));
    for(const [id,item] of jobs){task.append(text('p',`${item.state==='done'?'Готово':item.state==='running'?'Обработка':item.state==='skipped'?'Пропущено':'Ошибка'} · ${id} · ${item.backend||''} · ${item.message||''}`,'muted small'))}
  }catch(e){$('adminVideoDiagnostics').replaceChildren(text('p',e.message,'error'));$('adminHardwareReport').replaceChildren(text('p',e.message,'error'))}
  finally{button.disabled=false}
}
async function enqueueAdminVideo(){
  const id=$('adminVideoArchive').value,filename=$('adminVideoFile').value;
  if(!id||!filename)return;
  const btn=$('adminVideoConvert');btn.disabled=true;
  try{await api(`/api/admin/video/jobs/${encodeURIComponent(id)}`,{method:'POST',body:{filename}});toast('Перекодирование добавлено в очередь. Исходный файл сохранится.');await loadVideoReport(false)}
  catch(e){toast(e.message,true)}finally{btn.disabled=false}
}
async function loadAdminStorage(){
  if(!state.user?.admin)return;
  const target=$('adminStorageReport');target.replaceChildren(text('p','Проверяем место на диске…','muted'));
  try{
    const data=await api('/api/admin/storage');
    const format=bytes=>Number.isFinite(Number(bytes))?(Number(bytes)/(1024**3)).toFixed(2)+' ГиБ':'—';
    target.replaceChildren(text('strong','Всего: '+format(data.totalBytes)+' · свободно: '+format(data.freeBytes)));
    for(const u of data.users||[]){target.append(text('p',u.username+' · '+(u.error||format(u.bytes)),'muted small'))}
    if(Number(data.freeBytes)<Number(data.minFreeGiB)*1024**3)target.append(text('p','Свободное место ниже заданного резерва. Новые загрузки будут приостановлены.','error'));
  }catch(e){target.replaceChildren(text('p',e.message,'error'))}
}
function adminEvents(){
  $('adminStorageRefresh').onclick=loadAdminStorage;
  $('adminVideoProbe').onclick=()=>loadVideoReport(true);
  $('adminVideoArchive').onchange=selectAdminVideoArchive;
  $('adminVideoConvert').onclick=enqueueAdminVideo;
  $('adminSavePolicy').onclick=savePolicy;
  $('adminReloadPolicy').onclick=loadAdminPanel;
  $('adminUserPicker').onchange=selectPolicyUser;
  $('saveUserRights').onclick=saveUserRights;
}
