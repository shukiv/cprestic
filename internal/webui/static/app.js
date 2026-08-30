// Progressive enhancement only: every page works with JavaScript disabled.
(function () {
  "use strict";

  // Show the fields that belong to the chosen destination type.
  var typeSelect = document.querySelector("[data-destination-type]");
  if (typeSelect) {
    var sync = function () {
      document.querySelectorAll("[data-for-type]").forEach(function (block) {
        var applies = block.getAttribute("data-for-type") === typeSelect.value;
        block.hidden = !applies;
        block.querySelectorAll("input, select").forEach(function (field) {
          // A hidden required field would block submission invisibly.
          field.disabled = !applies;
        });
      });
    };
    typeSelect.addEventListener("change", sync);
    sync();
  }

  // Reveal the account picker only when the schedule is not "all accounts".
  document.querySelectorAll("[data-scope-toggle]").forEach(function (form) {
    var radios = form.querySelectorAll("input[name=scope]");
    var picker = form.querySelector("[data-account-picker]");
    if (!picker) return;
    var sync = function () {
      var selected = form.querySelector("input[name=scope]:checked");
      picker.hidden = !selected || selected.value !== "selected";
    };
    radios.forEach(function (radio) { radio.addEventListener("change", sync); });
    sync();
  });

  // Ask before anything that overwrites a live account.
  document.querySelectorAll("[data-confirm]").forEach(function (element) {
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
