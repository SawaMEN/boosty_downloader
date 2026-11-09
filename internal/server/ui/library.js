'use strict';
// Archive organization, author bookmarks and explicit read-only sharing.
let activeShareId = '';
let archiveFilter = '';
let authorsCache = [];
let sharedQuerySerial = 0;

function libraryAction(label, handler, cssClass = '') {
  const button = text('button', label, 'tiny-btn ' + cssClass);
  button.type = 'button';
  button.addEventListener('click', handler);
  return button;
}
async function loadAuthors() {
  if (!state.user) return;
  try {
    authorsCache = await api('/api/authors');
    const filter = $('archiveAuthorFilter');
    if (filter) {
      const selected = archiveFilter;
      filter.replaceChildren();
      const all = el('option'); all.value = ''; all.textContent = 'Все авторы'; filter.append(all);
      authorsCache.forEach(a => {
        const opt = el('option'); opt.value = a.name; opt.textContent = `${a.name} (${a.archives})`;
        filter.append(opt);
      });
      if (authorsCache.some(a => a.name === selected)) filter.value = selected;
      else { archiveFilter = ''; filter.value = ''; }
    }
    const pins = $('savedAuthors');
    if (pins) {
      pins.replaceChildren();
      const bookmarked = authorsCache.filter(a => a.pinned);
      if (!bookmarked.length) pins.append(empty('Пока нет избранных авторов', 'Сохраните имя автора, чтобы быстро возвращаться к его постам.'));
      bookmarked.forEach(a => {
        const card = el('article', 'author-tile');
        const name = el('div', 'author-name');
        name.append(text('strong', a.name));
        name.append(text('small', `${a.archives} архивных публикаций`, 'muted'));
        card.append(name);
        card.append(libraryAction('Публикации ↗', () => chooseAuthor(a.name)));
        card.append(libraryAction('Убрать', () => unpin(a.name), 'secondary'));
        pins.append(card);
      });
    }
    const active = state.feedAuthor;
    if ($('pinFeedAuthor')) {
      $('pinFeedAuthor').hidden = !active;
      $('pinFeedAuthor').textContent = authorsCache.some(a => a.name === active && a.pinned) ? '★ Автор сохранён' : '☆ Сохранить автора';
    }
  } catch(err) { toast(err.message, true); }
}
async function pinAuthor(name) {
  name = String(name || '').trim();
  if (!name) return toast('Укажите автора', true);
  try { await api('/api/authors', {method:'POST',body:{author:name}}); await loadAuthors(); toast('Автор добавлен в избранное'); }
  catch(e) { toast(e.message,true); }
}
async function unpin(name) {
  if (!confirm(`Удалить «${name}» из избранных? Сохранённые архивы останутся.`)) return;
  try { await api('/api/authors/'+encodeURIComponent(name),{method:'DELETE'}); await loadAuthors(); }
  catch(e) { toast(e.message,true); }
}
async function chooseAuthor(name) {
  $('feedAuthor').value = name;
  nav('browse');
  await loadFeed(true);
}
async function verifyArchiveUI(job) {
  try {
    toast('Проверяем контрольные суммы файлов…');
    const result = await api(`/api/jobs/${encodeURIComponent(job.id)}/verify`,{method:'POST'});
    if(result.valid) toast(result.partial?`Все ${result.verified} сохранённых файлов исправны, но копия публикации частичная`:`Архив проверен: ${result.verified} файлов, ошибок нет`,Boolean(result.partial));
    else toast(`Проверка: исправно ${result.verified}; повреждено ${result.damaged}; отсутствует ${result.missing}; без SHA-256 ${result.unchecked}. Для исправления нажмите «Обновить».`,true);
  } catch(err) { toast(err.message,true); }
}
async function deleteArchivedPost(job) {
  if (!confirm(`Удалить локальную копию «${job.title||job.postId}» (${job.author})? Совместный доступ также будет отозван. Восстановить удалённые файлы нельзя.`)) return;
  try {
    const result = await api('/api/jobs/'+encodeURIComponent(job.id), {method:'DELETE'});
    toast(`Удалено архивов: ${result.deleted}`);
    await refreshJobs(); await loadAuthors(); await refreshShared();
  } catch(e) { toast(e.message,true); }
}
async function deleteAuthorArchives() {
  const author = $('archiveAuthorFilter').value;
  if(!author) return toast('Выберите автора для удаления',true);
  const expected = `Удалить ВСЕ ваши архивы автора ${author}, включая видео, файлы и комментарии. Общий доступ будет отозван. Введите имя автора для подтверждения:`;
  const typed = prompt(expected);
  if (typed === null) return;
  if (typed !== author) return toast('Имя автора не совпадает, удаление отменено',true);
  try {
    const r = await api(`/api/authors/${encodeURIComponent(author)}/archives`, {method:'DELETE',body:{confirm:typed}});
    toast(`Удалено публикаций автора ${author}: ${r.deleted}`);
    archiveFilter=''; await refreshJobs(); await loadAuthors(); await refreshShared();
  } catch(e) { toast(e.message,true); }
}
async function refreshShared() {
  if (!state.user) return;
  const serial=++sharedQuerySerial;
  const author=$('sharedAuthorSearch')?.value.trim()||'';
  const q=$('sharedTextSearch')?.value.trim()||'';
  const query=new URLSearchParams();if(author)query.set('author',author);if(q)query.set('q',q);
  const status=$('sharedSearchStatus');if(status)status.textContent='Поиск архивов…';
  try {
    const list=await api('/api/shared'+(query.size?'?'+query.toString():''));
    if(serial!==sharedQuerySerial)return; // Out-of-order responses must not overwrite a new query.
    const node=$('sharedJobs');if(!node)return;
    node.replaceChildren();
    if(status)status.textContent=`Найдено архивов: ${list.length}`;
    if(!list.length)node.append(empty('Архивы не найдены', author||q?'Попробуйте другое имя автора или название поста.':'Общие архивы других участников появятся здесь после публикации.'));
    else list.forEach(j=>node.append(buildJob(j,true)));
  } catch(e) { if(serial===sharedQuerySerial){if(status)status.textContent='Ошибка загрузки';toast(e.message,true)} }
}
async function openShareDialog(job) {
  activeShareId=job.id;
  $('shareDialogTitle').textContent = 'Доступ: ' + (job.title||job.postId);
  $('shareUsername').value = '';
  $('shareDialog').hidden=false;
  await refreshShares();
}
async function refreshShares() {
  if (!activeShareId) return;
  const list = $('shareParticipants'); list.replaceChildren();
  try {
    const result=await api(`/api/jobs/${encodeURIComponent(activeShareId)}/shares`);
    $('shareEveryone').checked=Boolean(result.everyone);
    $('shareEveryone').disabled=!permissionValue('sharePublic') && !result.everyone;
    $('shareForm').hidden=!permissionValue('shareDirect');
    if (!result.users.length) list.append(text('p',result.everyone?'Индивидуальных разрешений пока нет. Общий доступ включён.':'Других пользователей с доступом пока нет.','muted'));
    result.users.forEach(username=>{
      const row=el('div','participant-row');row.append(text('strong',username));
      row.append(libraryAction('Отозвать', async()=>{
        if (!confirm(`Отозвать доступ у ${username}?`)) return;
        try { await api(`/api/jobs/${encodeURIComponent(activeShareId)}/shares`,{method:'DELETE',body:{username}}); await refreshShares();toast('Доступ отозван'); }
        catch(e){toast(e.message,true)}
      },'delete'));
      list.append(row);
    });
  } catch(e) { list.append(text('p',e.message,'error')); }
}
async function addShare(event) {
  event.preventDefault();
  const username=$('shareUsername').value.trim();
  if (!username || !activeShareId) return;
  try {
    await api(`/api/jobs/${encodeURIComponent(activeShareId)}/shares`,{method:'POST',body:{username}});
    $('shareUsername').value='';await refreshShares();toast('Доступ предоставлен');
  } catch(e) { toast(e.message,true); }
}
function libraryEvents() {
  $('archiveAuthorFilter').addEventListener('change',ev=>{archiveFilter=ev.target.value;renderLibraryJobs()});
  $('deleteAuthorArchives').addEventListener('click',deleteAuthorArchives);
  $('saveAuthorForm').addEventListener('submit',e=>{e.preventDefault();pinAuthor($('saveAuthorInput').value);$('saveAuthorInput').value=''});
  $('pinFeedAuthor').addEventListener('click',()=>pinAuthor(state.feedAuthor));
  $('refreshShared').addEventListener('click',refreshShared);
  $('sharedSearchForm').addEventListener('submit',e=>{e.preventDefault();refreshShared()});
  let searchTimer;
  for(const id of ['sharedAuthorSearch','sharedTextSearch'])$(id).addEventListener('input',()=>{clearTimeout(searchTimer);searchTimer=setTimeout(refreshShared,320)});
  $('resetSharedSearch').addEventListener('click',()=>{$('sharedAuthorSearch').value='';$('sharedTextSearch').value='';refreshShared()});
  $('shareEveryone').addEventListener('change',async e=>{
    if(!activeShareId)return;
    const enabled=e.target.checked;e.target.disabled=true;
    try{await api(`/api/jobs/${encodeURIComponent(activeShareId)}/visibility`,{method:'PUT',body:{everyone:enabled}});await refreshShares();await refreshJobs();toast(enabled?'Копия доступна всем пользователям':'Общий доступ отключён');}
    catch(err){e.target.checked=!enabled;toast(err.message,true)}finally{e.target.disabled=false}
  });
  $('shareForm').addEventListener('submit',addShare);
  $('closeShare').addEventListener('click',()=>{$('shareDialog').hidden=true;activeShareId=''});
  $('shareDialog').addEventListener('click',e=>{if(e.target===$('shareDialog')){$('shareDialog').hidden=true;activeShareId=''}});
}
