// Progressive enhancement. Every page works without this file.
(() => {
  // Timestamps arrive in UTC; show them in the viewer's locale and time zone,
  // relative when recent. The full date and time is in the tooltip.
  const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
  const hm = new Intl.DateTimeFormat(undefined, { timeStyle: "short" });
  const hms = new Intl.DateTimeFormat(undefined, { timeStyle: "medium" });
  const md = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" });
  const ymd = new Intl.DateTimeFormat(undefined, { dateStyle: "medium" });
  const full = new Intl.DateTimeFormat(undefined, { dateStyle: "full", timeStyle: "long" });
  const startOfDay = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate());
  const friendly = (t, now) => {
    const mins = Math.round((now - t) / 60000);
    if (mins >= 0 && mins < 1) return rtf.format(0, "second");
    if (mins >= 0 && mins < 60) return rtf.format(-mins, "minute");
    const days = Math.round((startOfDay(now) - startOfDay(t)) / 86400000);
    if (days === 0 || days === 1) return `${rtf.format(-days, "day")}, ${hm.format(t)}`;
    if (t.getFullYear() === now.getFullYear()) return `${md.format(t)}, ${hm.format(t)}`;
    return ymd.format(t);
  };
  const localizeTimes = (root) => {
    const now = new Date();
    for (const el of root.querySelectorAll("time[datetime]")) {
      const t = new Date(el.dateTime);
      if (isNaN(t)) continue;
      el.textContent = el.hasAttribute("data-clock") ? hms.format(t) : friendly(t, now);
      el.title = full.format(t);
    }
  };
  localizeTimes(document);
  setInterval(() => localizeTimes(document), 60000);

  const editor = document.querySelector("[data-editor]");
  if (editor) {
    const text = editor.querySelector("textarea");
    const initial = text.value;
    let submitting = false;

    // Cursor at the end, not the start.
    text.setSelectionRange(text.value.length, text.value.length);

    // Ctrl/Cmd+Enter: save and next (or "Save anyway" when shown). Approved
    // strings have no save buttons for translators.
    const primary = editor.querySelector("[data-save-next]") || editor.querySelector("button.primary");
    if (primary) {
      editor.querySelector("[data-hint]").hidden = !primary.matches("[data-save-next]");
      text.addEventListener("keydown", (e) => {
        if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
          e.preventDefault();
          primary.click();
        }
      });
    }

    const copy = editor.querySelector("[data-copy-source]");
    if (copy) {
      copy.hidden = false;
      copy.addEventListener("click", () => {
        if (text.readOnly) return;
        text.value = document.getElementById("source").textContent;
        text.focus();
      });
    }

    // Unchecking "Approved" unlocks the text right away instead of after saving.
    const approve = editor.querySelector("[data-approve]");
    if (approve) {
      approve.addEventListener("change", () => {
        if (!approve.checked && text.readOnly) {
          text.readOnly = false;
          text.focus();
        }
      });
    }

    editor.addEventListener("submit", () => { submitting = true; });
    window.addEventListener("beforeunload", (e) => {
      if (!submitting && text.value !== initial) e.preventDefault();
    });
  }

  // Key list beside the editor: keep its scroll position from page to page,
  // and bring the current key into view when it isn't.
  const keys = document.querySelector("[data-keys] ol");
  if (keys) {
    try {
      keys.scrollTop = Number(sessionStorage.getItem("keys-scroll")) || 0;
    } catch {}
    const current = keys.querySelector("[aria-current]");
    if (current) {
      const top = current.offsetTop - keys.offsetTop;
      if (top < keys.scrollTop || top + current.offsetHeight > keys.scrollTop + keys.clientHeight) {
        keys.scrollTop = top - keys.clientHeight / 2;
      }
    }
    window.addEventListener("pagehide", () => {
      try {
        sessionStorage.setItem("keys-scroll", keys.scrollTop);
      } catch {}
    });
  }

  // Filter the list while typing: fetch the same page with the new query and
  // swap in the results. Enter still submits the form normally.
  const search = document.querySelector("[data-search]");
  if (search) {
    const input = search.querySelector("input[type=search]");
    let timer, pending;
    const update = async () => {
      const url = new URL(search.action);
      url.search = new URLSearchParams(new FormData(search));
      if (!input.value.trim()) url.searchParams.delete("q");
      pending?.abort();
      pending = new AbortController();
      try {
        const res = await fetch(url, { signal: pending.signal });
        if (!res.ok) return;
        const doc = new DOMParser().parseFromString(await res.text(), "text/html");
        for (const sel of ["[data-results]", ".tabs"]) {
          const next = doc.querySelector(sel);
          if (next) {
            localizeTimes(next);
            document.querySelector(sel)?.replaceWith(next);
          }
        }
        history.replaceState(null, "", url);
      } catch (e) {
        if (e.name !== "AbortError") throw e;
      }
    };
    input.addEventListener("input", () => {
      clearTimeout(timer);
      timer = setTimeout(update, 200);
    });
  }

  // Click to select tokens and links meant for copying.
  for (const el of document.querySelectorAll("[data-select]")) {
    el.addEventListener("focus", () => el.select());
  }

  // Row menus close on a click elsewhere or Escape, like native menus.
  const menus = document.querySelectorAll("details.menu");
  if (menus.length) {
    const close = (keep) => {
      for (const m of menus) if (m !== keep) m.open = false;
    };
    document.addEventListener("click", (e) => close(e.target.closest("details.menu")));
    document.addEventListener("keydown", (e) => {
      const open = [...menus].find((m) => m.open);
      if (e.key === "Escape" && open) {
        close();
        open.querySelector("summary").focus();
      }
    });
  }

  // Pages showing work in progress, like a running AI run, reload until
  // it's done.
  const reload = document.querySelector("[data-reload]");
  if (reload) setTimeout(() => location.reload(), Number(reload.dataset.reload) * 1000);

  // Choices that change what a page shows apply right away.
  for (const el of document.querySelectorAll("[data-autosubmit]")) {
    el.addEventListener("change", () => el.form.requestSubmit());
  }

  for (const el of document.querySelectorAll("[data-confirm]")) {
    el.addEventListener("click", (e) => {
      if (!confirm(el.dataset.confirm)) e.preventDefault();
    });
  }
})();
