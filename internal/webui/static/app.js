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
  if (createAccount && adminFields) {
    createAccount.addEventListener("change", function () {
      adminFields.hidden = !createAccount.checked;
    });
  }

  // Show the fields that belong to the chosen destination type.
  var typeSelect = document.querySelector("[data-destination-type]");
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
  document.addEventListener("click", function (event) {
    var opener = event.target.closest("[data-dialog]");
    if (opener) {
      var dialog = document.getElementById(opener.dataset.dialog);
      if (dialog && dialog.showModal) {
        event.preventDefault();
        dialog.showModal();
      }
      return;
    }
    var closer = event.target.closest("[data-dialog-close]");
    if (closer) {
      var open = closer.closest("dialog");
      if (open) { open.close(); }
    }
  });
})();

// Theme control. WHM does not tell a plugin which theme it is wearing, so
// the operator gets the final say; "system" follows prefers-color-scheme.
// The choice lives in this browser only — it is a viewing preference, not
// server state.
(function () {
  "use strict";
  var group = document.getElementById("theme");
  if (!group) { return; }

  function stored() {
    try { return localStorage.getItem("cprest.theme") || "system"; } catch (e) { return "system"; }
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
