'use strict';
// Archive organization, author bookmarks and explicit read-only sharing.
let activeShareId = '';
let archiveFilter = '';
let authorsCache = [];
let sharedQuerySerial = 0;

let libraryQuerySerial = 0;
let libraryCatalogPage = 1;
let sharedCatalogPage = 1;
let libraryCatalogPages = 0;
let sharedCatalogPages = 0;
const CATALOG_PAGE_SIZE = 20;
function catalogSort(id) {
  const sort=$(id)?.value||'updated';
  return {sort,order:sort==='author'||sort==='title'?'asc':'desc'};
}
function catalogPageControls(prefix,page,pages) {
  $(prefix+'Prev').disabled=page<=1;
  $(prefix+'Next').disabled=pages===0||page>=pages;
  $(prefix+'PageInfo').textContent=pages?'Страница '+page+' из '+pages:'Страниц нет';
}
async function refreshLibraryCatalog(reset=false) {
  if(!state.user)return;
  if(reset)libraryCatalogPage=1;
  const serial=++libraryQuerySerial;
  const qs=new URLSearchParams({scope:'mine',page:String(libraryCatalogPage),perPage:String(CATALOG_PAGE_SIZE),...catalogSort('archiveSort')});
  const author=$('archiveAuthorFilter').value;
  const q=$('archiveTextSearch').value.trim();
  const status=$('archiveStateFilter').value;
  if(author)qs.set('author',author);
  if(q)qs.set('q',q);
  if(status)qs.set('status',status);
  $('archiveSearchStatus').textContent='Поиск архивов…';
  try {
    const page=await api('/api/catalog?'+qs.toString());
    if(serial!==libraryQuerySerial)return;
    libraryCatalogPages=page.pages;
    const node=$('libraryJobs');node.replaceChildren();
    if(!page.items.length)node.append(empty('Архивы не найдены','Измените фильтр или сохраните публикацию.'));
    else page.items.forEach(j=>node.append(buildJob(j)));
    $('archiveSearchStatus').textContent='Найдено: '+page.total;
    catalogPageControls('archive',page.page,page.pages);
    $('deleteAuthorArchives').disabled=!author;
  }catch(e){if(serial===libraryQuerySerial){$('archiveSearchStatus').textContent='Ошибка поиска';toast(e.message,true)}}
}


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
  if (!confirm(`Переместить «${job.title||job.postId}» (${job.author}) в корзину? Общий доступ будет отозван, архив можно восстановить.`)) return;
  try {
    const result = await api('/api/jobs/'+encodeURIComponent(job.id), {method:'DELETE'});
    toast(`Перемещено в корзину: ${result.trashed}`);
    await refreshJobs(); await loadAuthors(); await refreshShared(); await refreshTrash();
  } catch(e) { toast(e.message,true); }
}
async function deleteAuthorArchives() {
  const author = $('archiveAuthorFilter').value;
  if(!author) return toast('Выберите автора для удаления',true);
  const expected = `Переместить ВСЕ ваши архивы автора ${author} в корзину? Общий доступ будет отозван. Введите имя автора для подтверждения:`;
  const typed = prompt(expected);
  if (typed === null) return;
  if (typed !== author) return toast('Имя автора не совпадает, удаление отменено',true);
  try {
    const r = await api(`/api/authors/${encodeURIComponent(author)}/archives`, {method:'DELETE',body:{confirm:typed}});
    toast(`Перемещено в корзину публикаций автора ${author}: ${r.trashed}`);
    archiveFilter=''; await refreshJobs(); await loadAuthors(); await refreshShared(); await refreshTrash();
  } catch(e) { toast(e.message,true); }
}

async function refreshShared(reset=false) {
  if(!state.user)return;
  if(reset)sharedCatalogPage=1;
  const serial=++sharedQuerySerial;
  const author=$('sharedAuthorSearch')?.value.trim()||'';
  const q=$('sharedTextSearch')?.value.trim()||'';
  const qs=new URLSearchParams({scope:'shared',page:String(sharedCatalogPage),perPage:String(CATALOG_PAGE_SIZE),...catalogSort('sharedSort')});
  if(author)qs.set('author',author);
  if(q)qs.set('q',q);
  $('sharedSearchStatus').textContent='Поиск архивов…';
  try {
    const page=await api('/api/catalog?'+qs.toString());
    if(serial!==sharedQuerySerial)return;
    sharedCatalogPages=page.pages;
    const node=$('sharedJobs');node.replaceChildren();
    if(!page.items.length)node.append(empty('Архивы не найдены','Уточните автора или название публикации.'));
    else page.items.forEach(j=>node.append(buildJob(j,true)));
    $('sharedSearchStatus').textContent='Найдено: '+page.total;
    catalogPageControls('shared',page.page,page.pages);
  }catch(e){if(serial===sharedQuerySerial){$('sharedSearchStatus').textContent='Ошибка загрузки';toast(e.message,true)}}
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
async function refreshTrash() {
  const panel=$('trashPanel');
  if(!panel)return;
  panel.hidden=!permissionValue('deleteArchives');
  if(panel.hidden)return;
  const node=$('trashJobs');
  try {
    const jobs=await api('/api/trash');
    node.replaceChildren();
    if(!jobs.length){node.append(empty('Корзина пуста','Удалённые архивы появятся здесь.'));return;}
    for(const job of jobs){
      const row=el('article','job-item');
      const body=el('div','job-main');
      body.append(text('div',job.title||job.postId,'job-title'));
      body.append(text('div',job.author+' · удалён '+new Date(job.deletedAt).toLocaleString('ru-RU'),'job-sub'));
      const actions=el('div','job-buttons');
      actions.append(libraryAction('Восстановить',async()=>{
        try {await api('/api/trash/'+encodeURIComponent(job.id)+'/restore',{method:'POST'});toast('Архив восстановлен; прежний общий доступ не возвращён.');await refreshTrash();await refreshJobs();await loadAuthors();}
        catch(err){toast(err.message,true);}
      }));
      actions.append(libraryAction('Удалить навсегда',async()=>{
        if(!confirm('Окончательно удалить «'+(job.title||job.postId)+'» вместе с медиафайлами? Восстановить будет нельзя.'))return;
        try {await api('/api/trash/'+encodeURIComponent(job.id),{method:'DELETE'});toast('Архив удалён окончательно');await refreshTrash();}
        catch(err){toast(err.message,true);}
      },'delete'));
      body.append(actions);row.append(body);node.append(row);
    }
  }catch(err){node.replaceChildren();node.append(text('p','Не удалось загрузить корзину','error'));toast(err.message,true);}
}
function libraryEvents() {
  $('archiveAuthorFilter').addEventListener('change',ev=>{archiveFilter=ev.target.value;refreshLibraryCatalog(true)});
  let catalogTimer;
  $('archiveTextSearch').addEventListener('input',()=>{clearTimeout(catalogTimer);catalogTimer=setTimeout(()=>refreshLibraryCatalog(true),300)});
  for(const id of ['archiveStateFilter','archiveSort'])$(id).addEventListener('change',()=>refreshLibraryCatalog(true));
  $('archivePrev').addEventListener('click',()=>{if(libraryCatalogPage>1){libraryCatalogPage--;refreshLibraryCatalog()}});
  $('archiveNext').addEventListener('click',()=>{if(libraryCatalogPage<libraryCatalogPages){libraryCatalogPage++;refreshLibraryCatalog()}});
  $('deleteAuthorArchives').addEventListener('click',deleteAuthorArchives);
  $('refreshTrash').addEventListener('click',refreshTrash);
  $('saveAuthorForm').addEventListener('submit',e=>{e.preventDefault();pinAuthor($('saveAuthorInput').value);$('saveAuthorInput').value=''});
  $('pinFeedAuthor').addEventListener('click',()=>pinAuthor(state.feedAuthor));
  $('refreshShared').addEventListener('click',refreshShared);
  $('sharedSearchForm').addEventListener('submit',e=>{e.preventDefault();refreshShared(true)});
  let searchTimer;
  for(const id of ['sharedAuthorSearch','sharedTextSearch'])$(id).addEventListener('input',()=>{clearTimeout(searchTimer);searchTimer=setTimeout(()=>refreshShared(true),320)});
  $('resetSharedSearch').addEventListener('click',()=>{$('sharedAuthorSearch').value='';$('sharedTextSearch').value='';refreshShared(true)});
  $('sharedSort').addEventListener('change',()=>refreshShared(true));
  $('sharedPrev').addEventListener('click',()=>{if(sharedCatalogPage>1){sharedCatalogPage--;refreshShared()}});
  $('sharedNext').addEventListener('click',()=>{if(sharedCatalogPage<sharedCatalogPages){sharedCatalogPage++;refreshShared()}});
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
