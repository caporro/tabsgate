// Native modal confirmation, shared by connection and key actions. Opening or
// cancelling it leaves the current view and any unsaved form values intact.
const dialog = document.getElementById('tg-confirm-dialog');
const title = document.getElementById('tg-dialog-title');
const message = document.getElementById('tg-dialog-message');
const accept = document.getElementById('tg-dialog-accept');
const cancel = document.getElementById('tg-dialog-cancel');

accept.addEventListener('click', () => dialog.close('confirm'));
cancel.addEventListener('click', () => dialog.close('cancel'));

export function dismissConfirmation() {
  if (dialog.open) dialog.close('cancel');
}

export function confirmAction({ heading, description, label, destructive = false }) {
  if (dialog.open) return Promise.resolve(false);
  title.textContent = heading;
  message.textContent = description;
  accept.textContent = label;
  accept.classList.toggle('tg-destructive', destructive);
  accept.disabled = false;
  cancel.disabled = false;
  dialog.returnValue = '';
  return new Promise(resolve => {
    dialog.addEventListener('close', () => resolve(dialog.returnValue === 'confirm'), { once: true });
    dialog.showModal();
    cancel.focus();
  });
}
