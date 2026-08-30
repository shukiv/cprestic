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
      var pressed = chips.querySelector('.chip[aria-pressed="true"]');
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

  Array.prototype.forEach.call(document.querySelectorAll("[data-filter-state]"), function (group) {
    var target = group.dataset.filterState;
    group.addEventListener("click", function (event) {
      var chip = event.target.closest(".chip");
      if (!chip) { return; }
      Array.prototype.forEach.call(group.querySelectorAll(".chip"), function (other) {
        other.setAttribute("aria-pressed", String(other === chip));
      });
      applyFilters(target);
    });
  });

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

  // While work is in flight, refresh so progress appears without a click.
  if (document.querySelector("[data-running]")) {
    window.setTimeout(function () { window.location.reload(); }, 10000);
  }
})();
