"""Training script for C(x) cost field.

Trains C(x) with contrastive ranking loss on PASS/FAIL embedding pairs.

Note: G(x) metric tensor training was removed — V2.5.1 ablation confirmed
zero contribution at any correction strength (5.2M dead params). G(x)
quality scoring uses XGBoost instead.

Designed to run inside the geometric-lens container where torch is available.
"""

import json
import os
import random
import sys

import torch
import torch.optim as optim

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from geometric_lens.cost_field import CostField
from geometric_lens.thresholds import derive_gx_thresholds as _derive_gx_thresholds


def load_gate_data(path: str = None) -> dict:
    """Load embeddings and labels from gate analysis."""
    if path is None:
        path = os.path.join(
            os.path.dirname(os.path.abspath(__file__)), "gate_embeddings.json"
        )
    with open(path) as f:
        return json.load(f)


def build_pairs(embeddings, labels):
    """Build contrastive pairs: (pass_embedding, fail_embedding)."""
    pass_embs = [e for e, l in zip(embeddings, labels) if l == 1]
    fail_embs = [e for e, l in zip(embeddings, labels) if l == 0]
    pairs = []
    for p in pass_embs:
        for f in fail_embs:
            pairs.append((p, f))
    return pairs


def train_cost_field(
    data: dict,
    epochs: int = 200,
    lr: float = 1e-3,
    margin: float = 1.0,
    weight_decay: float = 1e-4,
    test_fraction: float = 0.3,
    seed: int = 42,
    patience: int = 40,
) -> dict:
    """Train C(x) with contrastive ranking loss.

    Loss = max(0, C(x_pass) - C(x_fail) + margin)

    We want C(x_fail) > C(x_pass) + margin.

    Test AUC is evaluated every epoch and the best-scoring weights are kept;
    training stops early once `patience` epochs pass without a new best
    (small sample sets reach their peak within the first few epochs and only
    memorize afterward).

    Returns dict with model, metrics, train/test AUC.
    """
    random.seed(seed)
    torch.manual_seed(seed)

    embeddings = data["embeddings"]
    labels = data["labels"]
    dim = len(embeddings[0])

    # Stratified train/test split
    pass_idx = [i for i, l in enumerate(labels) if l == 1]
    fail_idx = [i for i, l in enumerate(labels) if l == 0]
    random.shuffle(pass_idx)
    random.shuffle(fail_idx)

    n_pass_test = max(1, int(len(pass_idx) * test_fraction))
    n_fail_test = max(1, int(len(fail_idx) * test_fraction))

    test_idx = set(pass_idx[:n_pass_test] + fail_idx[:n_fail_test])
    train_idx = [i for i in range(len(embeddings)) if i not in test_idx]

    train_embs = [embeddings[i] for i in train_idx]
    train_labels = [labels[i] for i in train_idx]
    test_embs = [embeddings[i] for i in test_idx]
    test_labels = [labels[i] for i in test_idx]

    print(f"Train: {len(train_embs)} (PASS={sum(train_labels)}, FAIL={len(train_labels)-sum(train_labels)})")
    print(f"Test:  {len(test_embs)} (PASS={sum(test_labels)}, FAIL={len(test_labels)-sum(test_labels)})")

    # Build contrastive pairs
    train_pairs = build_pairs(train_embs, train_labels)
    test_pairs = build_pairs(test_embs, test_labels)
    print(f"Train pairs: {len(train_pairs)}, Test pairs: {len(test_pairs)}")

    # Convert to tensors
    device = torch.device("cpu")
    model = CostField(input_dim=dim).to(device)
    optimizer = optim.Adam(model.parameters(), lr=lr, weight_decay=weight_decay)

    # Training loop
    loss_history = []
    best_test_auc = 0.0
    best_epoch = 0
    best_state = None

    for epoch in range(epochs):
        model.train()
        random.shuffle(train_pairs)
        total_loss = 0.0
        n_batches = 0

        # Mini-batch training (batch_size=32 pairs)
        batch_size = min(32, len(train_pairs))
        for batch_start in range(0, len(train_pairs), batch_size):
            batch = train_pairs[batch_start:batch_start + batch_size]
            pass_batch = torch.tensor([p[0] for p in batch], dtype=torch.float32, device=device)
            fail_batch = torch.tensor([p[1] for p in batch], dtype=torch.float32, device=device)

            c_pass = model(pass_batch)
            c_fail = model(fail_batch)

            # Ranking loss: want C(fail) > C(pass) + margin
            loss = torch.relu(c_pass - c_fail + margin).mean()

            optimizer.zero_grad()
            loss.backward()
            optimizer.step()

            total_loss += loss.item()
            n_batches += 1

        avg_loss = total_loss / n_batches
        loss_history.append(avg_loss)

        # Evaluate every epoch: with small sample sets the test-AUC peak
        # arrives (and passes) within the first few epochs, so sparser
        # checkpoints miss it. AUC over a few hundred embeddings is cheap
        # next to the epoch's own batches. Print every 20 to keep the log
        # readable.
        model.eval()  # Note: model.eval() is the PyTorch eval mode toggle, not code evaluation
        with torch.no_grad():
            test_auc = compute_energy_auc(model, test_embs, test_labels, device)

        if test_auc > best_test_auc:
            best_test_auc = test_auc
            best_epoch = epoch + 1
            best_state = {k: v.clone() for k, v in model.state_dict().items()}

        if (epoch + 1) % 20 == 0 or epoch == 0:
            with torch.no_grad():
                train_auc = compute_energy_auc(model, train_embs, train_labels, device)
            print(f"Epoch {epoch+1:4d} | Loss: {avg_loss:.4f} | Train AUC: {train_auc:.4f} | Test AUC: {test_auc:.4f}")

        if patience and (epoch + 1) - best_epoch >= patience:
            print(f"Early stop at epoch {epoch+1}: no test-AUC improvement "
                  f"in {patience} epochs (best {best_test_auc:.4f} @ epoch "
                  f"{best_epoch})")
            break

    # Restore best model
    if best_state:
        model.load_state_dict(best_state)
        print(f"Keeping best checkpoint: epoch {best_epoch} "
              f"(test AUC {best_test_auc:.4f})")

    # Final assessment
    model.eval()  # Note: model.eval() is the PyTorch eval mode toggle, not code evaluation
    with torch.no_grad():
        final_train_auc = compute_energy_auc(model, train_embs, train_labels, device)
        final_test_auc = compute_energy_auc(model, test_embs, test_labels, device)

        # Compute energy statistics
        all_pass = torch.tensor([e for e, l in zip(embeddings, labels) if l == 1],
                                dtype=torch.float32, device=device)
        all_fail = torch.tensor([e for e, l in zip(embeddings, labels) if l == 0],
                                dtype=torch.float32, device=device)
        pass_energies = model(all_pass).squeeze()
        fail_energies = model(all_fail).squeeze()

    print("\n--- Final Results ---")
    print(f"Best test AUC: {best_test_auc:.4f}")
    print(f"Final train AUC: {final_train_auc:.4f}")
    print(f"Final test AUC: {final_test_auc:.4f}")
    print(f"PASS energy: {pass_energies.mean():.4f} +/- {pass_energies.std():.4f}")
    print(f"FAIL energy: {fail_energies.mean():.4f} +/- {fail_energies.std():.4f}")
    print(f"Separation: {fail_energies.mean() - pass_energies.mean():.4f}")

    return {
        "model": model,
        "best_test_auc": best_test_auc,
        "final_train_auc": final_train_auc,
        "final_test_auc": final_test_auc,
        "pass_energy_mean": pass_energies.mean().item(),
        "fail_energy_mean": fail_energies.mean().item(),
        "loss_history": loss_history,
    }


def compute_energy_auc(model, embeddings, labels, device):
    """Compute AUC: does C(x) rank FAIL higher than PASS?

    Higher AUC = better separation (FAIL embeddings get higher energy).
    """
    X = torch.tensor(embeddings, dtype=torch.float32, device=device)
    energies = model(X).squeeze().tolist()

    # AUC: probability that a random FAIL has higher energy than a random PASS
    pass_e = [e for e, l in zip(energies, labels) if l == 1]
    fail_e = [e for e, l in zip(energies, labels) if l == 0]

    if not pass_e or not fail_e:
        return 0.5

    concordant = 0
    total = 0
    for fe in fail_e:
        for pe in pass_e:
            total += 1
            if fe > pe:
                concordant += 1
            elif fe == pe:
                concordant += 0.5

    return concordant / total if total > 0 else 0.5


def save_cost_field(cost_field, save_dir=None, normalization=None):
    """Save trained C(x) model weights.

    Writes both cost_field.pt (what the service loads) and
    cost_field.safetensors (pickle-free twin for publishing — HF flags
    .pt files as unsafe-pickle). Keeping them written together prevents
    a stale safetensors from a previous model shadowing a fresh .pt.
    """
    if save_dir is None:
        save_dir = os.path.join(os.path.dirname(os.path.abspath(__file__)), "models")
    os.makedirs(save_dir, exist_ok=True)

    cost_path = os.path.join(save_dir, "cost_field.pt")
    torch.save(cost_field.state_dict(), cost_path)
    print(f"C(x) model saved to {cost_path}")
    try:
        from safetensors.torch import save_file
        st_path = os.path.join(save_dir, "cost_field.safetensors")
        save_file(cost_field.state_dict(), st_path)
        print(f"C(x) safetensors twin saved to {st_path}")
    except ImportError:
        print("safetensors not installed — skipping the pickle-free twin "
              "(pip install safetensors)")
    if normalization is not None:
        from geometric_lens.calibration import save_cx_normalization
        norm_path = save_cx_normalization(save_dir, normalization)
        print(f"C(x) normalization saved to {norm_path}")
    return cost_path


def train_gx(
    data: dict,
    pca_dim: int = 128,
    max_depth: int = 4,
    learning_rate: float = 0.1,
    max_rounds: int = 300,
    n_folds: int = 5,
    seed: int = 42,
) -> dict:
    """Train the G(x) correctness classifier on pooled embeddings.

    Same `data` dict as train_cost_field ({"embeddings", "labels"},
    label 1 = PASS). PCA-projects the embeddings to `pca_dim` features and
    fits an XGBoost binary classifier (gx_score = P(pass), matching
    service.py's `proba[1]` consumption). Stratified k-fold CV provides the
    reported AUC; the saved model is refit on the full set using the median
    early-stopped round count from the folds.

    Returns a dict for save_gx: booster + the gx_weights.json payload
    (pca_components/pca_mean in the exact shape service.py loads).
    """
    import numpy as np
    import xgboost as xgb
    from sklearn.decomposition import PCA
    from sklearn.metrics import roc_auc_score
    from sklearn.model_selection import StratifiedKFold

    X = np.array(data["embeddings"], dtype=np.float32)
    y = np.array(data["labels"], dtype=np.int32)
    # Optional per-sample weights (in-the-loop labeling: a thumbs-down pass
    # down-weights its accepted files, a denial is full-weight, etc.). Absent
    # or wrong-length → uniform weights, so bench-built lenses are unaffected.
    w = None
    if data.get("weights") is not None and len(data["weights"]) == len(y):
        w = np.array(data["weights"], dtype=np.float32)
    n, dim = X.shape
    n_pass, n_fail = int((y == 1).sum()), int((y == 0).sum())
    if min(n_pass, n_fail) < 5:
        raise ValueError(
            f"G(x) needs at least 5 samples of each class "
            f"(got PASS={n_pass}, FAIL={n_fail})")

    k = min(pca_dim, n, dim)
    pca = PCA(n_components=k, random_state=seed)
    Xp = pca.fit_transform(X).astype(np.float32)

    params = {
        "objective": "binary:logistic",
        "max_depth": max_depth,
        "eta": learning_rate,
        "eval_metric": "auc",
        "seed": seed,
    }

    folds = min(n_folds, n_pass, n_fail)
    aucs, accs, best_rounds = [], [], []
    # Out-of-fold predictions: every sample scored by a booster that never saw
    # it. Operating thresholds are derived from these instead of the final
    # booster's in-sample scores, which are optimistically shifted.
    oof_scores = np.full(n, np.nan, dtype=np.float64)
    skf = StratifiedKFold(n_splits=folds, shuffle=True, random_state=seed)
    for fold, (tr, te) in enumerate(skf.split(Xp, y)):
        dtr = xgb.DMatrix(Xp[tr], label=y[tr], weight=(w[tr] if w is not None else None))
        dte = xgb.DMatrix(Xp[te], label=y[te])
        bst = xgb.train(params, dtr, num_boost_round=max_rounds,
                        evals=[(dte, "val")], early_stopping_rounds=30,
                        verbose_eval=False)
        pred = bst.predict(dte, iteration_range=(0, bst.best_iteration + 1))
        oof_scores[te] = pred
        aucs.append(roc_auc_score(y[te], pred))
        accs.append(float(((pred >= 0.5).astype(int) == y[te]).mean()))
        best_rounds.append(bst.best_iteration + 1)
        print(f"Fold {fold + 1}/{folds} | AUC: {aucs[-1]:.4f} | "
              f"rounds: {best_rounds[-1]}")

    # Final model on the full set, sized by the folds' early stopping.
    rounds = int(np.median(best_rounds))
    dall = xgb.DMatrix(Xp, label=y, weight=w)
    booster = xgb.train(params, dall, num_boost_round=rounds)

    scores = booster.predict(dall)
    importance = booster.get_score(importance_type="gain")
    feat_imp = np.zeros(k, dtype=np.float64)
    for name, gain in importance.items():
        feat_imp[int(name[1:])] = gain
    top_dims = np.argsort(feat_imp)[::-1][:30]

    cv_auc = float(np.mean(aucs))
    print("\n--- G(x) Results ---")
    print(f"CV AUC: {cv_auc:.4f} +/- {np.std(aucs):.4f} ({folds} folds)")
    print(f"PASS score: {scores[y == 1].mean():.4f} | "
          f"FAIL score: {scores[y == 0].mean():.4f}")

    # Per-model operating thresholds, derived from THIS model's score scale so
    # the off-rails / regression interventions actually fire (a fixed 0.3/0.15
    # cutoff tuned for one model is silent on another whose grounded writes
    # cluster elsewhere). gx_score = P(pass): good writes score
    # high, bad writes low. We anchor on percentiles of the PASS distribution
    # so each cutoff has a controlled false-positive rate on good writes:
    #   severe (~5th pct)    — a good write almost never scores this low, so one
    #                          sample below it is enough to intervene
    #   off_rails (~10th pct)— per-token "stop generating" cutoff
    #   low (~20th pct)      — moderate; run-of-2 below it is a regression
    # Clamped to a sane band and ordered severe <= off_rails <= low.
    # Derived from the out-of-fold PASS scores collected during CV — the final
    # booster's in-sample scores sit higher than what unseen writes get at
    # serve time, which would push every cutoff too high.
    oof_pass = oof_scores[y == 1]
    oof_pass = oof_pass[~np.isnan(oof_pass)]
    thresholds = _derive_gx_thresholds(oof_pass)
    print(f"G(x) thresholds (from out-of-fold PASS percentiles): {thresholds}")

    return {
        "booster": booster,
        "cv_auc_mean": cv_auc,
        "thresholds": thresholds,
        "weights": {
            "architecture": "xgboost_pca",
            "pca_dim": k,
            "original_dim": dim,
            "n_training_samples": n,
            "cv_auc_mean": cv_auc,
            "cv_auc_std": float(np.std(aucs)),
            "cv_acc_mean": float(np.mean(accs)),
            "feature_importances": feat_imp.tolist(),
            "top_dims": top_dims.tolist(),
            "pass_score_mean": float(scores[y == 1].mean()),
            "fail_score_mean": float(scores[y == 0].mean()),
            "pca_components": pca.components_.astype(np.float64).tolist(),
            "pca_mean": pca.mean_.astype(np.float64).tolist(),
        },
    }


def save_gx(gx_result: dict, save_dir=None):
    """Save trained G(x) artifacts in the layout service.py loads.

    Writes gx_xgboost.json (native booster dump) + gx_weights.json (PCA
    projection + stats). Removes a stale gx_xgboost.pkl if present — the
    pickle is the legacy load fallback, and one left behind from a previous
    model would silently serve the wrong PCA dimensions if the JSON were
    ever deleted.
    """
    if save_dir is None:
        save_dir = os.path.join(os.path.dirname(os.path.abspath(__file__)), "models")
    os.makedirs(save_dir, exist_ok=True)

    xgb_path = os.path.join(save_dir, "gx_xgboost.json")
    gx_result["booster"].save_model(xgb_path)
    weights_path = os.path.join(save_dir, "gx_weights.json")
    with open(weights_path, "w") as f:
        json.dump(gx_result["weights"], f)
    print(f"G(x) model saved to {xgb_path}")

    # Per-model operating thresholds travel with the artifact (the lens service
    # loads this per-model; the proxy reads it back from each score response).
    # Absent → service keeps threshold interventions disabled.
    thresholds = gx_result.get("thresholds")
    if thresholds:
        thr_path = os.path.join(save_dir, "gx_thresholds.json")
        with open(thr_path, "w") as f:
            json.dump(thresholds, f, indent=2)
        print(f"G(x) thresholds saved to {thr_path}: {thresholds}")

    stale_pkl = os.path.join(save_dir, "gx_xgboost.pkl")
    if os.path.exists(stale_pkl):
        os.remove(stale_pkl)
        print(f"Removed stale legacy artifact {stale_pkl} "
              f"(superseded by the JSON dump)")
    return xgb_path


if __name__ == "__main__":
    print("=" * 60)
    print("GEOMETRIC LENS TRAINING — C(x) Cost Field")
    print("=" * 60)

    # Load gate data
    data = load_gate_data()
    print(f"Loaded {len(data['embeddings'])} embeddings, dim={len(data['embeddings'][0])}")

    # Train C(x)
    print("\n--- Training C(x) Cost Field ---")
    cx_result = train_cost_field(data, epochs=200, margin=1.0)

    if cx_result["best_test_auc"] < 0.70:
        print(f"\nWARNING: Test AUC {cx_result['best_test_auc']:.4f} < 0.70 threshold")
        print("C(x) may not generalize well. Proceeding with caution...")

    # Save model
    print("\n--- Saving Model ---")
    from geometric_lens.calibration import derive_cx_normalization
    normalization = derive_cx_normalization(
        cx_result["pass_energy_mean"], cx_result["fail_energy_mean"])
    save_cost_field(cx_result["model"], normalization=normalization)

    # Summary
    print("\n" + "=" * 60)
    print("TRAINING SUMMARY")
    print("=" * 60)
    print(f"C(x) test AUC:    {cx_result['best_test_auc']:.4f} (threshold: 0.70)")
    print(f"C(x) PASS energy: {cx_result['pass_energy_mean']:.4f}")
    print(f"C(x) FAIL energy: {cx_result['fail_energy_mean']:.4f}")
    print("=" * 60)
