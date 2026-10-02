
    // 与管理台的浅色/深色选择保持一致；未设置时跟随系统
    try {
      const savedTheme = localStorage.getItem('icloud_hme_theme');
      if (savedTheme === 'light' || savedTheme === 'dark') document.documentElement.dataset.theme = savedTheme;
    } catch (e) {
      // 隐私模式下读取受限时跟随系统主题
    }

    let emailList = [];
    try {
      const dataEl = document.getElementById('mailViewData');
      emailList = JSON.parse(dataEl ? (dataEl.dataset.items || '[]') : '[]');
    } catch (e) {
      console.error('邮件列表数据解析失败:', e);
    }
    let currentIdx = 0;
    const activeSec = 3;
    const bgSec = 10;
    let pollIntervalSec = activeSec;
    let remaining = pollIntervalSec;
    let totalSec = pollIntervalSec;
    let autoRefreshPaused = false;
    let isChecking = false;

    function showToast(msg) {
      const el = document.getElementById('toast');
      if (!el) return;
      el.textContent = msg;
      el.classList.add('show');
      clearTimeout(window.__toastTimer);
      window.__toastTimer = setTimeout(function() {
        el.classList.remove('show');
      }, 2500);
    }

    async function copyText(str, msg) {
      if (!str) return;
      let copied = false;
      if (navigator.clipboard && navigator.clipboard.writeText) {
        try {
          await navigator.clipboard.writeText(str);
          copied = true;
        } catch (e) {
          // Browser permissions may deny clipboard writes; try legacy copy.
        }
      }
      if (!copied) {
        const focused = document.activeElement;
        const ta = document.createElement('textarea');
        ta.value = str;
        ta.style.position = 'fixed';
        ta.style.opacity = '0';
        document.body.appendChild(ta);
        try {
          ta.select();
          copied = document.execCommand('copy') === true;
        } catch (e) {
          copied = false;
        } finally {
          ta.remove();
          if (focused && typeof focused.focus === 'function') focused.focus();
        }
      }
      showToast(copied ? (msg || '已复制') : '复制失败，请选择文本手动复制');
    }

    function copyCode(code) {
      if (!code) return;
      copyText(code, '验证码 ' + code + ' 已复制');
    }

    function copyCurrentUrl() {
      copyText(window.location.href, '查信链接已复制');
    }

    function copyFullBody() {
      const item = emailList[currentIdx];
      if (item) {
        copyText(item.text_body || item.html_body, '邮件全文已复制');
      }
    }

    function switchTab(viewId) {
      document.querySelectorAll('.tab-btn').forEach(function(b) { b.classList.remove('active'); });
      document.querySelectorAll('.body-view').forEach(function(v) { v.classList.remove('active'); });
      const btn = document.getElementById('tab-' + viewId);
      const view = document.getElementById('view-' + viewId);
      if (btn) btn.classList.add('active');
      if (view) view.classList.add('active');
    }

    function selectMail(index, preserveView) {
      if (!emailList || emailList.length === 0) return;
      if (index < 0 || index >= emailList.length) index = 0;
      const activeTab = preserveView ? document.querySelector('.tab-btn.active') : null;
      currentIdx = index;

      document.querySelectorAll('.mail-item').forEach(function(el) { el.classList.remove('active'); });
      const activeEl = document.getElementById('mailItem-' + index);
      if (activeEl) activeEl.classList.add('active');

      const item = emailList[index];
      if (!item) return;

      const bodyStatus = document.getElementById('bodyStatus');
      if (bodyStatus) {
        bodyStatus.textContent = item.body_error ? '正文读取失败：' + item.body_error : (item.body_complete === false ? '当前仅提供邮件摘要预览，非完整正文' : '');
        bodyStatus.classList.toggle('is-hidden', !bodyStatus.textContent);
      }

      const subEl = document.getElementById('detailSubject');
      if (subEl) subEl.textContent = item.subject || '（无主题）';
      const avEl = document.getElementById('detailAvatar');
      if (avEl) avEl.textContent = item.sender_initial || 'M';
      const sNameEl = document.getElementById('detailSenderName');
      if (sNameEl) sNameEl.textContent = item.sender_name || item.from;
      const sMailEl = document.getElementById('detailSenderEmail');
      if (sMailEl) sMailEl.textContent = item.sender_email || item.from;
      const dDateEl = document.getElementById('detailDate');
      if (dDateEl) dDateEl.textContent = item.formatted_date || item.date;

      // Update OTP Box
      const otpBox = document.getElementById('detailOtpBox');
      const otpCode = document.getElementById('detailOtpCode');
      const magicBtn = document.getElementById('detailMagicBtn');
      if (item.has_otp && (item.code || item.magic_link)) {
        if (otpBox) otpBox.classList.remove('is-hidden');
        if (otpCode) otpCode.textContent = item.code || '';
        if (magicBtn) {
          if (item.magic_link) {
            magicBtn.classList.remove('is-hidden');
            magicBtn.href = item.magic_link;
          } else {
            magicBtn.classList.add('is-hidden');
            magicBtn.removeAttribute('href');
          }
        }
      } else if (otpBox) {
        otpBox.classList.add('is-hidden');
        if (magicBtn) {
          magicBtn.classList.add('is-hidden');
          magicBtn.removeAttribute('href');
        }
      }
      ['detailOtpCodeBox', 'detailOtpCopyBtn'].forEach(function(id) {
        const el = document.getElementById(id);
        if (el) el.classList.toggle('is-hidden', !item.code);
      });

      // Update raw text
      const rawArea = document.getElementById('rawContentArea');
      if (rawArea) rawArea.textContent = item.text_body || item.html_body || '';

      // Update iframe
      const frame = document.getElementById('mailFrame');
      if (frame) {
        if (item.is_html) {
          // 独立 HTTP 文档使用邮件专属 CSP，避免 srcdoc 继承主页面的内联样式限制。
          const dataEl = document.getElementById('mailViewData');
          const currentURL = new URL(window.location.href);
          const previewURL = new URL('/mail/raw', currentURL.origin);
          previewURL.searchParams.set('email', dataEl.dataset.email);
          previewURL.searchParams.set('account_id', dataEl.dataset.accountId);
          previewURL.searchParams.set('message_id', item.id);
          previewURL.searchParams.set('format', 'html');
          previewURL.searchParams.set('frame', '1');
          ['token', 'api_key'].forEach(function(key) {
            if (currentURL.searchParams.has(key)) previewURL.searchParams.set(key, currentURL.searchParams.get(key));
          });
          frame.removeAttribute('srcdoc');
          if (frame.src !== previewURL.href) frame.src = previewURL.href;
        } else {
          frame.removeAttribute('src');
          frame.srcdoc = '<!DOCTYPE html><html><body><pre>' + escapeHtml(item.text_body) + '</pre></body></html>';
        }
      }

      // Update styled plain text view
      renderStyledText(item);

      if (activeTab) {
        switchTab(activeTab.id.replace('tab-', ''));
      } else if (item.is_html) {
        switchTab('html');
      } else {
        switchTab('styled');
      }
    }

    function escapeHtml(raw) {
      if (!raw) return '';
      return String(raw).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    function escapeAttr(raw) {
      return escapeHtml(raw);
    }

    function renderStyledText(item) {
      const target = document.getElementById('styledContentArea');
      if (!target || !item) return;
      const raw = item.text_body || '';
      const content = document.createDocumentFragment();
      const tokens = /https?:\/\/[^\s<>"')]+|\b\d{4,8}\b/g;
      let offset = 0;
      for (const match of raw.matchAll(tokens)) {
        content.appendChild(document.createTextNode(raw.slice(offset, match.index)));
        const token = match[0];
        let node = document.createTextNode(token);
        if (token.startsWith('http')) {
          try {
            const url = new URL(token);
            const link = document.createElement('a');
            link.href = url.href;
            link.title = token;
            link.target = '_blank';
            link.rel = 'noopener noreferrer';
            link.className = 'link-badge';
            link.textContent = url.hostname.replace(/^www\./, '');
            node = link;
          } catch (e) {
            // Invalid URLs remain readable plain text.
          }
        } else if (item.has_otp && token === item.code) {
          const code = document.createElement('span');
          code.className = 'code-highlight';
          code.textContent = token;
          node = code;
        }
        content.appendChild(node);
        offset = match.index + token.length;
      }
      content.appendChild(document.createTextNode(raw.slice(offset)));
      target.replaceChildren(content);
    }

    document.addEventListener('click', function(e) {
      const target = e.target.closest('[data-action]');
      if (!target) return;
      const action = target.dataset.action;
      if (action === 'select-mail') {
        selectMail(Number(target.dataset.index));
      } else if (action === 'copy-code') {
        e.stopPropagation();
        copyCode(target.dataset.code || '');
      } else if (action === 'copy-email') {
        copyText(target.dataset.email || '', '已复制别名邮箱');
      } else if (action === 'copy-url') {
        copyCurrentUrl();
      } else if (action === 'copy-latest-otp') {
        const latest = document.getElementById('topLatestOtp');
        copyCode(latest ? latest.textContent.trim() : '');
      } else if (action === 'copy-detail-otp') {
        const code = document.getElementById('detailOtpCode');
        copyCode(code ? code.textContent.trim() : '');
      } else if (action === 'copy-full-body') {
        copyFullBody();
      } else if (action === 'toggle-refresh') {
        toggleAutoRefresh();
      } else if (action === 'refresh' || action === 'manual-refresh') {
        doRefresh();
      } else if (action === 'switch-tab') {
        switchTab(target.dataset.view || 'styled');
      }
    });


    // SILENT LIVE BACKGROUND POLLING
    async function checkNewMailsSilently(isManual) {
      if (isChecking) {
        if (isManual) showToast('正在同步中，请稍候...');
        return;
      }
      isChecking = true;

      const secEl = document.getElementById('countdownSec');
      if (secEl) secEl.textContent = '检测中...';
      const spin = document.getElementById('refreshSpin');
      if (spin) spin.classList.add('is-spinning');

      try {
        const u = new URL(window.location.href);
        u.searchParams.set('format', 'json');
        u.searchParams.set('_t', Date.now().toString());

        const res = await fetch(u.toString(), {
          headers: { 'Accept': 'application/json' },
          cache: 'no-store'
        });

        if (res.ok) {
          const resp = await res.json();
          if (resp && resp.success && resp.data) {
            applyNewData(resp.data, isManual);
          }
        } else if (res.status === 401 || res.status === 403) {
          autoRefreshPaused = true;
          const pill = document.getElementById('refreshPill');
          if (pill) pill.textContent = '权限已失效';
          showToast('身份验证失效，已暂停自动检测');
        } else if (isManual) {
          showToast('同步邮件失败，请稍后重试');
        }
      } catch(err) {
        console.warn('Silent poll error:', err);
      } finally {
        isChecking = false;
        if (spin) spin.classList.remove('is-spinning');
        remaining = totalSec;
        if (secEl && !autoRefreshPaused) secEl.textContent = remaining + 's';
      }
    }

    function applyNewData(data, isManual) {
      if (!data) return;

      const selectedID = emailList[currentIdx] ? emailList[currentIdx].id : '';
      const newItems = data.items || [];
      const listContentChanged = JSON.stringify(emailList) !== JSON.stringify(newItems);
      const oldFirstID = (emailList && emailList.length > 0) ? emailList[0].id : '';
      const newFirstID = (data.items && data.items.length > 0) ? data.items[0].id : '';
      const oldLatestOtp = (emailList && emailList.find(function(x) { return x.has_otp; })) ? emailList.find(function(x) { return x.has_otp; }).code : '';
      const newLatestOtp = (data.all_otps && data.all_otps.length > 0) ? data.all_otps[0].code : '';

      const listCountChanged = Boolean((emailList ? emailList.length : 0) !== (data.items ? data.items.length : 0));
      const hasNewMail = Boolean(newFirstID && newFirstID !== oldFirstID);
      const hasNewOTP = Boolean(newLatestOtp && newLatestOtp !== oldLatestOtp);

      emailList = newItems;

      // Update Top Master OTP Bar (Extract latest OTP from AllOTPs)
      const masterOtpBar = document.getElementById('otpMasterBar');
      const latestOtpItem = (data.all_otps && data.all_otps.length > 0) ? data.all_otps[0] : null;
      const fav = document.getElementById('pageFavicon');

      if (latestOtpItem) {
        if (masterOtpBar) masterOtpBar.classList.remove('is-hidden');
        const topLatestOtp = document.getElementById('topLatestOtp');
        if (topLatestOtp) {
          topLatestOtp.textContent = latestOtpItem.code || '';
          topLatestOtp.classList.toggle('is-hidden', !latestOtpItem.code);
        }
        const topCopyBtn = document.getElementById('topLatestCopyBtn');
        if (topCopyBtn) topCopyBtn.classList.toggle('is-hidden', !latestOtpItem.code);
        const topLabel = document.getElementById('topOtpLabel');
        if (topLabel) topLabel.textContent = latestOtpItem.code ? '提取到验证码' : '提取到激活链接';

        const topMagicBtn = document.getElementById('topMagicBtn');
        if (topMagicBtn) {
          if (latestOtpItem.magic_link) {
            topMagicBtn.classList.remove('is-hidden');
            topMagicBtn.href = latestOtpItem.magic_link;
          } else {
            topMagicBtn.classList.add('is-hidden');
            topMagicBtn.removeAttribute('href');
          }
        }

        if (latestOtpItem && latestOtpItem.code) {
          if (fav) {
            fav.href = "data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'><circle cx='16' cy='16' r='14' fill='%2310b981'/><path fill='%23ffffff' d='M14 20.5l-4.5-4.5 1.4-1.4 3.1 3.1 7.6-7.6 1.4 1.4z'/></svg>";
          }
        }

        const historySec = document.getElementById('otpHistorySection');
        if (historySec) {
          if (data.all_otps.length > 1) {
            historySec.classList.remove('is-hidden');
            let chipsHTML = '<span class="otp-history-label">全部验证信息 · ' + data.all_otps.length + ' 条</span>';
            data.all_otps.forEach(function(it) {
              if (!it.code) {
                chipsHTML += '<a class="otp-chip-btn" href="' + escapeAttr(it.magic_link) + '" target="_blank" rel="noopener noreferrer">激活链接 · ' + escapeHtml(it.sender_name) + '</a>';
                return;
              }
              chipsHTML += '<button class="otp-chip-btn" data-action="copy-code" data-code="' + escapeAttr(it.code) + '">' +
                '<span class="otp-chip-code">' + escapeHtml(it.code) + '</span>' +
                '<span>· ' + escapeHtml(it.sender_name) + ' (' + escapeHtml(it.relative_date) + ')</span>' +
                '</button>';
            });
            historySec.innerHTML = chipsHTML;
          } else {
            historySec.classList.add('is-hidden');
          }
        }
      } else if (masterOtpBar) {
        masterOtpBar.classList.add('is-hidden');
        if (fav) {
          fav.href = "data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'><path fill='%232563eb' d='M26 15a7 7 0 0 0-13.4-2.8A5 5 0 0 0 4 17a5 5 0 0 0 5 5h17a5 5 0 0 0 0-10z'/><path fill='%2338bdf8' d='M18 10l-4 6h3v5l5-7h-4z'/></svg>";
        }
      }

      // Update document title (Clean standard mailbox format: 收件箱 (N) · email)
      if (data.has_mail && (data.total_count > 0 || (emailList && emailList.length > 0))) {
        const count = data.total_count || (emailList ? emailList.length : 0);
        document.title = '收件箱 (' + count + ') · ' + (data.email || '');
      } else {
        document.title = '收件箱 · ' + (data.email || '');
      }

      // Update status badge
      const statusBadge = document.querySelector('.status-badge');
      if (statusBadge) {
        if (data.has_mail) {
          statusBadge.className = 'status-badge live';
          statusBadge.innerHTML = '<span class="pulse-dot"></span> 实时就绪';
        } else {
          statusBadge.className = 'status-badge idle';
          statusBadge.innerHTML = '<span class="pulse-dot"></span> 实时监听中';
        }
      }

      // Transition between empty state and mail grid
      const emptyCard = document.getElementById('emptyCard');
      const mainGrid = document.getElementById('mainGrid');
      if (data.has_mail) {
        if (emptyCard) emptyCard.classList.add('is-hidden');
        if (mainGrid) mainGrid.classList.remove('is-hidden');
      } else {
        if (emptyCard) emptyCard.classList.remove('is-hidden');
        if (mainGrid) mainGrid.classList.add('is-hidden');
      }

      // Update Left List Badge
      const countBadge = document.getElementById('inboxCountBadge');
      if (countBadge) countBadge.textContent = '共 ' + (data.total_count || 0) + ' 封';

      // Refresh content updates too, preserving the selected mail and reading tab.
      const selectTop = hasNewMail || hasNewOTP || listCountChanged;
      if (selectTop || listContentChanged) {
        const selectedIndex = emailList.findIndex(function(it) { return it.id === selectedID; });
        const nextIndex = selectTop || selectedIndex < 0 ? 0 : selectedIndex;
        const listContainer = document.getElementById('mailListContainer');
        if (listContainer && emailList.length > 0) {
          let listHTML = '';
          emailList.forEach(function(it) {
            const activeCls = (it.index === nextIndex) ? ' active' : '';
            let otpBadge = '<div class="item-preview">' + escapeHtml(it.subject) + '</div>';
            if (it.code) {
              otpBadge = '<div class="item-otp-pill"><span>' + escapeHtml(it.code) + '</span>' +
                '<span class="quick-copy" data-action="copy-code" data-code="' + escapeAttr(it.code) + '">复制</span></div>';
            }
            listHTML += '<div class="mail-item' + activeCls + '" id="mailItem-' + it.index + '" data-action="select-mail" data-index="' + it.index + '">' +
              '<div class="mail-item-top">' +
                '<div class="sender-box">' +
                  '<div class="mini-avatar">' + escapeHtml(it.sender_initial || 'M') + '</div>' +
                  '<span class="mini-sender">' + escapeHtml(it.sender_name) + '</span>' +
                '</div>' +
                '<span class="mini-time">' + escapeHtml(it.relative_date) + '</span>' +
              '</div>' +
              '<div class="mail-item-subject">' + escapeHtml(it.subject) + '</div>' +
              '<div class="mail-item-bottom">' + otpBadge + '</div>' +
            '</div>';
          });
          listContainer.innerHTML = listHTML;
        }

        if (selectTop) {
          if (newLatestOtp) {
            showToast('🎉 收到新邮件！最新验证码: ' + newLatestOtp);
          } else {
            showToast('🎉 收到新邮件！收件列表已更新');
          }
        } else if (isManual) {
          showToast('邮件内容已更新');
        }
        selectMail(nextIndex, !selectTop && selectedIndex >= 0);
      } else {
        // No new mail arrived: keep user reading state intact, give feedback if manual check
        if (isManual) {
          showToast('已完成检查，暂无新邮件');
        }
      }
    }

    function toggleAutoRefresh() {
      autoRefreshPaused = !autoRefreshPaused;
      const pill = document.getElementById('refreshPill');
      if (pill) {
        pill.innerHTML = autoRefreshPaused ? '自动检测已暂停' : '<span id="countdownSec">' + Math.max(0, remaining) + 's</span> 后自动检测';
      }
      showToast(autoRefreshPaused ? '自动检测已暂停' : '自动检测已开启');
    }

    function doRefresh() {
      checkNewMailsSilently(true);
    }

    setInterval(function() {
      if (autoRefreshPaused || isChecking) return;
      remaining--;
      if (remaining <= 0) {
        remaining = totalSec;
        checkNewMailsSilently(false);
      }
      const secEl = document.getElementById('countdownSec');
      if (secEl) secEl.textContent = remaining + 's';
      const prog = document.getElementById('emptyProgressBar');
      if (prog) {
        prog.style.width = Math.max(0, (remaining / totalSec) * 100) + '%';
      }
    }, 1000);

    // Initial render
    if (emailList && emailList.length > 0) {
      selectMail(0);
    }

    // Clean URL address bar: remove any temporary _t or format query params
    try {
      if (window.history && window.history.replaceState) {
        const u = new URL(window.location.href);
        if (u.searchParams.has('_t') || u.searchParams.has('format')) {
          u.searchParams.delete('_t');
          u.searchParams.delete('format');
          window.history.replaceState(null, '', u.pathname + (u.search ? u.search : ''));
        }
      }
    } catch(e) {}

    // Auto check when tab becomes active again and adapt polling frequency
    document.addEventListener('visibilitychange', function() {
      if (!document.hidden) {
        pollIntervalSec = activeSec;
        totalSec = activeSec;
        remaining = 0;
        checkNewMailsSilently(false);
      } else {
        pollIntervalSec = bgSec;
        totalSec = bgSec;
      }
    });

    // Keyboard shortcuts
    document.addEventListener('keydown', function(e) {
      if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;
      if (e.ctrlKey || e.metaKey || e.altKey) return;
      if (e.key === 'c' || e.key === 'C') {
        const item = emailList[currentIdx];
        if (item && item.has_otp) copyCode(item.code);
      } else if (e.key === 'r' || e.key === 'R') {
        doRefresh();
      } else if (e.key === 'ArrowDown') {
        if (currentIdx < emailList.length - 1) selectMail(currentIdx + 1);
      } else if (e.key === 'ArrowUp') {
        if (currentIdx > 0) selectMail(currentIdx - 1);
      }
    });
