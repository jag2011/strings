// Progressive enhancement. Every page works without this file.
(() => {
  const editor = document.querySelector("[data-editor]");
  if (editor) {
    const text = editor.querySelector("textarea");
    const initial = text.value;
    let submitting = false;

    // Cursor at the end, not the start.
    text.setSelectionRange(text.value.length, text.value.length);

    // Ctrl/Cmd+Enter: save and next (or "Save anyway" when shown). Approved
    // strings have no save buttons for translators.
    const primary = editor.querySelector("button.primary");
    if (primary) {
      editor.querySelector("[data-hint]").hidden = false;
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
          if (next) document.querySelector(sel)?.replaceWith(next);
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

  for (const el of document.querySelectorAll("[data-confirm]")) {
    el.addEventListener("click", (e) => {
      if (!confirm(el.dataset.confirm)) e.preventDefault();
    });
  }
})();
