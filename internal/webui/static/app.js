// Progressive enhancement only: every page works with JavaScript disabled.
(function () {
  "use strict";

  // Account filtering happens here rather than on the server: the list is
  // already rendered, and a round trip per keystroke would be slower and
  // would cost a page load for something the browser can do instantly.
  function rowsOf(selector) {
    var table = document.querySelector(selector);
    return table ? Array.prototype.slice.call(table.querySelectorAll("tbody tr")) : [];
  }

  function applyFilters(selector) {
    var search = document.querySelector('[data-filter="' + selector + '"]');
    var chips = document.querySelector('[data-filter-state="' + selector + '"]');
    var term = search ? search.value.trim().toLowerCase() : "";
    var state = "";
    if (chips) {
      var pressed = chips.querySelector('.cpr-chip[aria-pressed="true"]');
      state = pressed ? pressed.dataset.state : "";
    }

    var shown = 0;
    rowsOf(selector).forEach(function (row) {
      var name = (row.dataset.name || "").toLowerCase();
      var matchesTerm = !term || name.indexOf(term) !== -1;
      var matchesState = !state || row.dataset.state === state;
      var visible = matchesTerm && matchesState;
      row.hidden = !visible;
      if (visible) { shown++; }
    });

    var counter = document.querySelector('[data-count-for="' + selector + '"]');
    if (counter) { counter.textContent = String(shown); }
  }

  Array.prototype.forEach.call(document.querySelectorAll("[data-filter]"), function (input) {
    var target = input.dataset.filter;
    input.addEventListener("input", function () { applyFilters(target); });
  });

  // A live refresh replaces rows, which loses the filter the operator set.
  // Re-applying it is the one thing outside this closure needs from here.
  window.cprestReapplyFilters = function () {
    Array.prototype.forEach.call(document.querySelectorAll("[data-filter]"), function (input) {
      applyFilters(input.dataset.filter);
    });
  };

  Array.prototype.forEach.call(document.querySelectorAll("[data-filter-state]"), function (group) {
    var target = group.dataset.filterState;
    group.addEventListener("click", function (event) {
      var chip = event.target.closest(".cpr-chip");
      if (!chip) { return; }
      Array.prototype.forEach.call(group.querySelectorAll(".cpr-chip"), function (other) {
        other.setAttribute("aria-pressed", String(other === chip));
      });
      applyFilters(target);
    });
  });

  // The administrator to log in as only matters when cprest is being
  // asked to create the account.
  var createAccount = document.querySelector("[data-quick-create]");
  var adminFields = document.querySelector("[data-quick-admin]");
  var passwordLabel = document.querySelector("[data-quick-password-label]");
  if (createAccount) {
    createAccount.addEventListener("change", function () {
      if (adminFields) { adminFields.hidden = !createAccount.checked; }
      // The same field means two different passwords, so it says which.
      if (passwordLabel) {
        passwordLabel.textContent = createAccount.checked
          ? "Administrator's password on that server"
          : "Password for that account";
      }
    });
  }

  // The recommended excludes are added to whatever is already there, and
  // only the lines that are not there yet — pressing the same one twice
  // should not fill the box with duplicates.
  var excludes = document.querySelector("[data-excludes]");
  Array.prototype.forEach.call(document.querySelectorAll("[data-exclude-preset]"), function (button) {
    button.addEventListener("click", function () {
      if (!excludes) { return; }
      var have = excludes.value.split("\n").map(function (line) { return line.trim(); });
      button.dataset.excludePreset.split("\n").forEach(function (line) {
        if (line && have.indexOf(line) === -1) {
          have.push(line);
        }
      });
      excludes.value = have.filter(function (line) { return line !== ""; }).join("\n");
      excludes.focus();
    });
  });

  // Picking files to restore. The list of what is chosen has to survive
  // walking into another folder, which is a page load, so it lives in the
  // textarea — the same field someone can still type into — and is kept
  // across those loads in this tab's own storage.
  var chosen = document.querySelector("[data-chosen-paths]");
  if (chosen) {
    var key = "cprest.chosen." + (chosen.dataset.chosenKey || "");

    var remember = function () {
      try { window.sessionStorage.setItem(key, chosen.value); } catch (e) {}
      var count = chosen.value.split("\n").filter(function (line) {
        return line.trim() !== "";
      }).length;
      Array.prototype.forEach.call(document.querySelectorAll("[data-chosen-count]"), function (node) {
        node.textContent = String(count);
      });
    };

    try {
      var kept = window.sessionStorage.getItem(key);
      if (kept && chosen.value === "") { chosen.value = kept; }
    } catch (e) {}
    remember();
    chosen.addEventListener("input", remember);

    var boxes = function () {
      return Array.prototype.slice.call(document.querySelectorAll("[data-choose]"));
    };

    var all = document.querySelector("[data-choose-all]");
    if (all) {
      all.addEventListener("change", function () {
        boxes().forEach(function (box) { box.checked = all.checked; });
      });
    }

    var add = document.querySelector("[data-choose-add]");
    if (add) {
      add.addEventListener("click", function () {
        var lines = chosen.value.split("\n").map(function (line) { return line.trim(); });
        boxes().forEach(function (box) {
          if (box.checked && lines.indexOf(box.dataset.choose) === -1) {
            lines.push(box.dataset.choose);
            box.checked = false;
          }
        });
        if (all) { all.checked = false; }
        chosen.value = lines.filter(function (line) { return line !== ""; }).join("\n");
        remember();
      });
    }

    // Once the restore is queued the list has served its purpose; leaving
    // it would repopulate the next one with the last one's paths.
    var form = chosen.form;
    if (form) {
      form.addEventListener("submit", function () {
        try { window.sessionStorage.removeItem(key); } catch (e) {}
      });
    }
  }

  // Show the fields that belong to the chosen destination type.
  var typeSelect = document.querySelector("[data-type-toggle]");
  if (typeSelect) {
    var syncType = function () {
      Array.prototype.forEach.call(document.querySelectorAll("[data-for-type]"), function (block) {
        var applies = block.getAttribute("data-for-type") === typeSelect.value;
        block.hidden = !applies;
        Array.prototype.forEach.call(block.querySelectorAll("input, select"), function (field) {
          // A hidden required field would block submission invisibly.
          field.disabled = !applies;
        });
      });
    };
    typeSelect.addEventListener("change", syncType);
    syncType();
  }

  // Reveal the account picker only when a schedule is not "all accounts".
  Array.prototype.forEach.call(document.querySelectorAll("[data-scope-toggle]"), function (form) {
    var picker = form.querySelector("[data-account-picker]");
    if (!picker) { return; }
    var syncScope = function () {
      var selected = form.querySelector("input[name=scope]:checked");
      picker.hidden = !selected || selected.value !== "selected";
    };
    Array.prototype.forEach.call(form.querySelectorAll("input[name=scope]"), function (radio) {
      radio.addEventListener("change", syncScope);
    });
    syncScope();
  });

  // Ask before anything that overwrites a live account.
  Array.prototype.forEach.call(document.querySelectorAll("[data-confirm]"), function (element) {
    element.addEventListener("submit", function (event) {
      if (!window.confirm(element.getAttribute("data-confirm"))) {
        event.preventDefault();
      }
    });
  });

})();

// Sortable tables.
//
// The server sends every table in the order that matters most — newest
// first — and that is the order an operator wants nine times out of ten.
// The tenth time they are asking a different question of the same rows:
// which account stored the most, which run failed, who has not been backed
// up. Doing that server-side would mean a round trip and a lost scroll
// position for a question answered from rows already on the page.
//
// What a cell shows and what it sorts by are not the same: "2 h ago" and
// "6.2 MiB new of 152.5 MiB" sort as text into nonsense, so those cells
// carry data-sort with a number in it. Anything without one sorts on its
// text, which is right for an account name.
(function () {
  "use strict";

  // Tables an operator has reordered, so a live refresh can put their
  // order back over the rows the server just sent.
  var chosen = [];

  function key(row, column) {
    var cell = row.children[column];
    if (!cell) { return ""; }
    var raw = cell.getAttribute("data-sort");
    return raw === null ? cell.textContent.trim() : raw;
  }

  function numeric(rows, column) {
    for (var i = 0; i < rows.length; i++) {
      var value = key(rows[i], column);
      if (value !== "" && isNaN(Number(value))) { return false; }
    }
    return rows.length > 0;
  }

  function apply(table, column, descending) {
    var body = table.tBodies[0];
    if (!body) { return; }
    var rows = Array.prototype.slice.call(body.rows);
    var asNumber = numeric(rows, column);
    rows.sort(function (a, b) {
      var left = key(a, column);
      var right = key(b, column);
      var order;
      if (asNumber) {
        order = Number(left) - Number(right);
      } else {
        order = left.localeCompare(right, undefined, { numeric: true, sensitivity: "base" });
      }
      return descending ? -order : order;
    });
    rows.forEach(function (row) { body.appendChild(row); });

    Array.prototype.forEach.call(table.tHead.rows[0].cells, function (cell, index) {
      if (index === column) {
        cell.setAttribute("aria-sort", descending ? "descending" : "ascending");
      } else if (cell.hasAttribute("aria-sort")) {
        cell.setAttribute("aria-sort", "none");
      }
    });
  }

  function remember(table, column, descending) {
    for (var i = 0; i < chosen.length; i++) {
      if (chosen[i].table === table) {
        chosen[i].column = column;
        chosen[i].descending = descending;
        return;
      }
    }
    chosen.push({ table: table, column: column, descending: descending });
  }

  function prepare(table) {
    if (!table.tHead || !table.tHead.rows.length) { return; }
    Array.prototype.forEach.call(table.tHead.rows[0].cells, function (cell, index) {
      if (cell.hasAttribute("data-nosort") || cell.textContent.trim() === "") { return; }
      var label = cell.textContent.trim();
      var button = document.createElement("button");
      button.type = "button";
      button.className = "cpr-sortbtn";
      button.textContent = label;
      cell.textContent = "";
      cell.appendChild(button);
      cell.setAttribute("aria-sort", "none");
      button.addEventListener("click", function () {
        // Same column again reverses it; a new column starts descending
        // for numbers and dates, ascending for names, which is what each
        // one is usually being asked for.
        var current = cell.getAttribute("aria-sort");
        var descending = current === "none"
          ? numeric(Array.prototype.slice.call(table.tBodies[0].rows), index)
          : current === "ascending";
        apply(table, index, descending);
        remember(table, index, descending);
      });
    });
  }

  Array.prototype.forEach.call(document.querySelectorAll("table[data-sortable]"), prepare);

  // Live refresh replaces the rows wholesale. Without this the operator's
  // chosen order silently reverts to the server's every three seconds.
  window.cprestReapplySort = function () {
    chosen.forEach(function (state) {
      if (document.contains(state.table)) {
        apply(state.table, state.column, state.descending);
      }
    });
  };
})();

// While work is in flight, keep the page current without reloading it.
//
// A full reload threw away the scroll position and any open report, which
// on a nineteen-account run meant the page jumped to the top every few
// seconds. Instead the same page is fetched, parsed here, and only the
// regions marked data-live are swapped. The response is HTML we already
// know how to render, so nothing depends on cpsrvd passing a content type
// through, which it does not.
(function () {
  "use strict";
  var INTERVAL = 3000;
  var timer = null;
  var inFlight = false;

  function running() { return document.querySelector("[data-running]") !== null; }

  function swap(fresh) {
    var swapped = false;
    Array.prototype.forEach.call(fresh.querySelectorAll("[data-live]"), function (node) {
      var here = document.querySelector('[data-live="' + node.dataset.live + '"]');
      if (here && here.innerHTML !== node.innerHTML) {
        here.innerHTML = node.innerHTML;
        swapped = true;
      }
    });
    if (swapped && typeof window.cprestReapplyFilters === "function") {
      // The rows are new, so the filter the operator typed has to be put
      // back over them.
      window.cprestReapplyFilters();
    }
    if (swapped && typeof window.cprestReapplySort === "function") {
      window.cprestReapplySort();
    }
  }

  function tick() {
    timer = null;
    if (inFlight) { return; }
    // A report the operator has open must not be pulled out from under
    // them; the next tick will catch up.
    if (document.querySelector("dialog[open]")) { schedule(); return; }

    inFlight = true;
    window.fetch(window.location.href, {
      credentials: "same-origin",
      headers: { "X-Cprest-Live": "1" }
    }).then(function (response) {
      return response.ok ? response.text() : null;
    }).then(function (html) {
      if (!html) { return; }
      var fresh = new DOMParser().parseFromString(html, "text/html");
      swap(fresh);
    }).catch(function () {
      // A refresh that fails is not worth reporting: the page still shows
      // what it last knew, and the next tick tries again.
    }).then(function () {
      inFlight = false;
      schedule();
    });
  }

  function schedule() {
    if (timer !== null || !running()) { return; }
    timer = window.setTimeout(tick, INTERVAL);
  }

  schedule();
})();

// A report is long and wanted occasionally, so it lives in a dialog rather
// than under every row.
(function () {
  "use strict";
  // What the drawer holds when nothing has been loaded into it: the form
  // for adding a destination. Editing borrows the same drawer and puts
  // this back on the way out.
  var drawerBody = document.querySelector("[data-drawer-body]");
  var drawerTitle = document.querySelector("[data-drawer-title]");
  var drawerHome = drawerBody ? { html: drawerBody.innerHTML, title: drawerTitle.textContent } : null;

  function focusFirst(dialog) {
    var first = dialog.querySelector("input:not([type=hidden]), select, textarea");
    if (first) { first.focus(); }
  }

  // Editing loads the form for that destination into the drawer. It is
  // fetched as the page it already is, and read here — the same trick the
  // live refresh uses, and for the same reason: nothing then depends on
  // cpsrvd passing a content type through, which it does not.
  function loadIntoDrawer(dialog, link) {
    drawerTitle.textContent = link.dataset.dialogTitle || drawerHome.title;
    drawerBody.innerHTML = '<p class="cpr-hint">Loading…</p>';
    dialog.showModal();

    window.fetch(link.href, { credentials: "same-origin" })
      .then(function (response) { return response.ok ? response.text() : null; })
      .then(function (html) {
        if (!html) { throw new Error("no page"); }
        var fetched = new DOMParser().parseFromString(html, "text/html");
        var content = fetched.querySelector("[data-drawer-content]");
        if (!content) { throw new Error("no form"); }
        drawerBody.innerHTML = content.innerHTML;
        focusFirst(dialog);
      })
      .catch(function () {
        // The form exists as its own page; if it cannot be brought here,
        // go to it rather than leaving an empty drawer.
        window.location.href = link.href;
      });
  }

  document.addEventListener("click", function (event) {
    var opener = event.target.closest("[data-dialog]");
    if (opener) {
      var dialog = document.getElementById(opener.dataset.dialog);
      if (dialog && dialog.showModal) {
        // Only then: without this the link still goes to the same form on
        // a page of its own, which is what a browser with no JavaScript
        // gets and what a shared link opens.
        event.preventDefault();
        if (opener.hasAttribute("data-dialog-fetch") && drawerBody) {
          loadIntoDrawer(dialog, opener);
          return;
        }
        dialog.showModal();
        focusFirst(dialog);
      }
      return;
    }
    // Clicking the backdrop is how a sheet is dismissed by everyone who
    // has ever used one.
    if (event.target.tagName === "DIALOG" && event.target.classList.contains("cpr-sheet")) {
      event.target.close();
      return;
    }
    var closer = event.target.closest("[data-dialog-close]");
    if (closer) {
      var open = closer.closest("dialog");
      if (open) { open.close(); }
    }
  });

  // A form that came back with something wrong reopens where it was being
  // filled in, rather than leaving the reason on a page behind it.
  var refused = document.querySelector(".cpr-sheet[data-drawer-open]");
  if (refused && refused.showModal) {
    refused.showModal();
    focusFirst(refused);
  }

  // Whatever was loaded in goes away with the drawer, so opening it again
  // is opening the same thing it was before.
  var drawer = document.querySelector(".cpr-sheet");
  if (drawer && drawerHome) {
    drawer.addEventListener("close", function () {
      drawerBody.innerHTML = drawerHome.html;
      drawerTitle.textContent = drawerHome.title;
    });
  }
})();

// Theme control. WHM does not tell a plugin which theme it is wearing, so
// the operator gets the final say. Light is the default — it is the theme
// this interface is designed in — and an operator who prefers dark, or who
// wants the machine's preference to decide, says so here. The choice lives
// in this browser only: it is a viewing preference, not server state.
(function () {
  "use strict";
  var group = document.getElementById("theme");
  if (!group) { return; }

  function stored() {
    var choice;
    try { choice = localStorage.getItem("cprest.theme"); } catch (e) { return "light"; }
    return (choice === "dark" || choice === "system") ? choice : "light";
  }

  function apply(choice) {
    if (choice === "light" || choice === "dark") {
      document.documentElement.setAttribute("data-theme", choice);
    } else {
      document.documentElement.removeAttribute("data-theme");
    }
    Array.prototype.forEach.call(group.querySelectorAll("button"), function (button) {
      button.setAttribute("aria-pressed", String(button.dataset.themeChoice === choice));
    });
    try { localStorage.setItem("cprest.theme", choice); } catch (e) {}
  }

  // Only offered when it can actually work.
  group.hidden = false;
  apply(stored());

  group.addEventListener("click", function (event) {
    var button = event.target.closest("[data-theme-choice]");
    if (button) { apply(button.dataset.themeChoice); }
  });
})();
